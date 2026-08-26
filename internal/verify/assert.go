package verify

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
)

// AssertionType is one kind of check run against a restored backup (SPEC §8).
type AssertionType string

const (
	// AssertTableCount compares how many tables came back with how many the
	// manifest recorded.
	AssertTableCount AssertionType = "table_count"
	// AssertRowCount compares the rows of each table with the manifest.
	AssertRowCount AssertionType = "row_count"
	// AssertQuery runs the operator's own SQL and compares the answer.
	AssertQuery AssertionType = "query"
	// AssertMaxAge fails a backup that is older than it should be.
	AssertMaxAge AssertionType = "max_age"
)

// Assertion is one configured check.
type Assertion struct {
	Type AssertionType
	// Tables narrows row_count to a few tables. Empty means every table the
	// manifest records.
	Tables []string
	// Tolerance is the accepted relative drift of a row count, 0 meaning
	// exact. Nil means the assertion said nothing, which is not the same as
	// zero — see toleranceFor.
	Tolerance *float64
	SQL       string
	Expect    any
	// MaxAge is how old the backup may be.
	MaxAge time.Duration
}

// Check is what one assertion found. It is written onto the manifest, so it
// says what was compared and not merely that something failed.
type Check struct {
	Type   AssertionType `json:"type"`
	Table  string        `json:"table,omitempty"`
	OK     bool          `json:"ok"`
	Detail string        `json:"detail"`
}

// estimateTolerance is the drift a row count is allowed when the number it is
// compared against is an estimate rather than a count.
//
// Row counts in a manifest are estimates by default (decision D7): the
// planner's statistics for PostgreSQL, information_schema for MySQL, both
// cheap and both approximate. Comparing an estimate for equality measures the
// age of the statistics, not the health of the backup. What a row_count
// assertion is really for is catching a restore that lost a table or emptied
// one, and this band catches that while staying quiet about sampling noise.
const estimateTolerance = 0.2

// runAssertions performs every configured check against the restored database.
//
// It collects results rather than stopping at the first failure, for the same
// reason config validation does: an operator looking at a broken backup wants
// the whole list, not the first item of it.
func runAssertions(
	ctx context.Context,
	in core.Inspector,
	m *manifest.Manifest,
	assertions []Assertion,
	now time.Time,
) ([]Check, error) {
	var checks []Check

	for _, assertion := range assertions {
		if err := ctx.Err(); err != nil {
			// A cancelled run has not found anything; reporting what it got to
			// before the deadline as the verdict would be a lie.
			return nil, err
		}

		switch assertion.Type {
		case AssertTableCount:
			checks = append(checks, tableCountCheck(ctx, in, m))
		case AssertRowCount:
			checks = append(checks, rowCountChecks(ctx, in, m, assertion)...)
		case AssertQuery:
			checks = append(checks, queryCheck(ctx, in, assertion))
		case AssertMaxAge:
			checks = append(checks, maxAgeCheck(m, assertion, now))
		default:
			checks = append(checks, Check{
				Type:   assertion.Type,
				Detail: fmt.Sprintf("unknown assertion type %q", assertion.Type),
			})
		}
	}

	return checks, nil
}

func tableCountCheck(ctx context.Context, in core.Inspector, m *manifest.Manifest) Check {
	tables, err := in.Tables(ctx)
	if err != nil {
		return Check{Type: AssertTableCount, Detail: "the restored tables could not be listed: " + err.Error()}
	}

	if len(tables) != len(m.Tables) {
		return Check{
			Type: AssertTableCount,
			Detail: fmt.Sprintf("the restored database holds %d tables, the manifest records %d",
				len(tables), len(m.Tables)),
		}
	}
	return Check{
		Type:   AssertTableCount,
		OK:     true,
		Detail: fmt.Sprintf("%d tables, as the manifest records", len(tables)),
	}
}

// rowCountChecks compares the rows of each selected table with the manifest,
// one check per table: which table lost its rows is the whole point.
func rowCountChecks(ctx context.Context, in core.Inspector, m *manifest.Manifest, a Assertion) []Check {
	if !hasRowCounts(m) {
		return []Check{{
			Type: AssertRowCount,
			Detail: "the manifest records no row counts to compare against; " +
				"set row_estimate to estimate or exact on this target, or drop the row_count assertion",
		}}
	}

	tables, missing := selectTables(m, a.Tables)

	checks := make([]Check, 0, len(tables)+len(missing))
	for _, name := range missing {
		checks = append(checks, Check{
			Type:   AssertRowCount,
			Table:  name,
			Detail: "the manifest records no table named " + name,
		})
	}

	for _, table := range tables {
		got, err := in.CountRows(ctx, table.Name)
		if err != nil {
			checks = append(checks, Check{
				Type:   AssertRowCount,
				Table:  table.Name,
				Detail: "the rows could not be counted: " + err.Error(),
			})
			continue
		}

		tolerance, estimated := toleranceFor(a, table)
		against := "counted"
		if estimated {
			against = fmt.Sprintf("estimated, within %.0f%%", tolerance*100)
		} else if tolerance > 0 {
			against = fmt.Sprintf("within %.0f%%", tolerance*100)
		}

		if !withinTolerance(table.Rows, got, tolerance) {
			checks = append(checks, Check{
				Type:  AssertRowCount,
				Table: table.Name,
				Detail: fmt.Sprintf("%s came back with %d rows, the manifest records %d (%s)",
					table.Name, got, table.Rows, against),
			})
			continue
		}
		checks = append(checks, Check{
			Type:   AssertRowCount,
			Table:  table.Name,
			OK:     true,
			Detail: fmt.Sprintf("%s: %d rows against %d in the manifest (%s)", table.Name, got, table.Rows, against),
		})
	}

	return checks
}

func queryCheck(ctx context.Context, in core.Inspector, a Assertion) Check {
	value, err := in.Scalar(ctx, a.SQL)
	if errors.Is(err, core.ErrQueryUnsupported) {
		return Check{Type: AssertQuery, Detail: "this engine has no queries to assert on"}
	}
	if err != nil {
		return Check{Type: AssertQuery, Detail: "the assertion query failed: " + err.Error()}
	}

	if !valuesEqual(value, a.Expect) {
		return Check{
			Type: AssertQuery,
			Detail: fmt.Sprintf("%s returned %s, the assertion expects %s",
				oneLine(a.SQL), renderValue(value), renderValue(a.Expect)),
		}
	}
	return Check{
		Type:   AssertQuery,
		OK:     true,
		Detail: fmt.Sprintf("%s returned %s, as expected", oneLine(a.SQL), renderValue(value)),
	}
}

// maxAgeCheck is the one assertion that needs no database: it reads the
// manifest. It belongs with the others because what it protects against —
// a backup that stopped being taken — looks fine from every other angle.
func maxAgeCheck(m *manifest.Manifest, a Assertion, now time.Time) Check {
	age := m.Age(now)
	if age > a.MaxAge {
		return Check{
			Type:   AssertMaxAge,
			Detail: fmt.Sprintf("the backup is %s old; the assertion allows %s", round(age), a.MaxAge),
		}
	}
	return Check{
		Type:   AssertMaxAge,
		OK:     true,
		Detail: fmt.Sprintf("the backup is %s old, within %s", round(age), a.MaxAge),
	}
}

// selectTables resolves the tables an assertion names against the manifest,
// reporting the names that match nothing. A name written without its schema
// still matches, because that is how people write them.
func selectTables(m *manifest.Manifest, names []string) (tables []core.TableInfo, missing []string) {
	if len(names) == 0 {
		return m.Tables, nil
	}

	for _, name := range names {
		found := false
		for _, table := range m.Tables {
			if table.Name == name || unqualified(table.Name) == name {
				tables = append(tables, table)
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, name)
		}
	}
	return tables, missing
}

// toleranceFor picks what "the same" means for one row count, and reports
// whether the manifest's number is an estimate.
//
// An assertion that sets a tolerance gets exactly that, including `tolerance:
// 0` meaning exact — the operator asked. An assertion that says nothing gets
// exact for a counted number and estimateTolerance for an estimated one.
func toleranceFor(a Assertion, table core.TableInfo) (tolerance float64, estimated bool) {
	if a.Tolerance != nil {
		return *a.Tolerance, false
	}
	if table.RowsExact {
		return 0, false
	}
	return estimateTolerance, true
}

func withinTolerance(want, got int64, tolerance float64) bool {
	if want == got {
		return true
	}
	if tolerance <= 0 {
		return false
	}
	drift := math.Abs(float64(got) - float64(want))
	return drift <= tolerance*math.Abs(float64(want))
}

// hasRowCounts reports whether the manifest holds anything to compare against.
// A target set to `row_estimate: off` records a table list and no numbers, and
// zero rows everywhere is indistinguishable from an empty database.
func hasRowCounts(m *manifest.Manifest) bool {
	for _, table := range m.Tables {
		if table.RowsExact || table.Rows > 0 {
			return true
		}
	}
	return false
}

// valuesEqual compares what a query returned with what the config expects.
//
// Both sides arrive as `any` from decoders free to pick their own types — YAML
// hands over a uint64 where a driver hands over an int64, and MySQL hands over
// a []byte — so they are compared by value rather than by Go type.
func valuesEqual(got, want any) bool {
	gotNumber, gotIsNumber := asNumber(got)
	wantNumber, wantIsNumber := asNumber(want)
	if gotIsNumber && wantIsNumber {
		return gotNumber == wantNumber
	}

	gotBool, gotIsBool := got.(bool)
	wantBool, wantIsBool := want.(bool)
	if gotIsBool && wantIsBool {
		return gotBool == wantBool
	}

	return renderValue(got) == renderValue(want)
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// renderValue is how a value reaches a report: short, and the same on both
// sides of a comparison.
func renderValue(v any) string {
	switch value := v.(type) {
	case nil:
		return "null"
	case string:
		return value
	case []byte:
		return string(value)
	case time.Time:
		return value.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprint(value)
	}
}

func unqualified(name string) string {
	if _, table, found := strings.Cut(name, "."); found {
		return table
	}
	return name
}

// oneLine flattens text for a single-line report: a SQL assertion is often
// written across three lines in the YAML.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// round keeps a duration readable in a report; a backup's age is never
// interesting to the microsecond.
func round(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(time.Minute)
}
