package verify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
)

// stubInspector is a restored database that answers whatever a test says it
// holds. The assertions are the unit under test here; what a real server would
// have replied is the engine adapters' business.
type stubInspector struct {
	tables    []string
	rows      map[string]int64
	value     any
	tablesErr error
	rowsErr   error
	valueErr  error
}

func (s stubInspector) Tables(context.Context) ([]string, error) {
	return s.tables, s.tablesErr
}

func (s stubInspector) CountRows(_ context.Context, table string) (int64, error) {
	if s.rowsErr != nil {
		return 0, s.rowsErr
	}
	return s.rows[table], nil
}

func (s stubInspector) Scalar(context.Context, string) (any, error) {
	return s.value, s.valueErr
}

func tolerance(value float64) *float64 { return &value }

func TestRunAssertions(t *testing.T) {
	now := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)

	// A backup taken three hours ago, of two tables whose row counts are the
	// planner's estimates — the default (decision D7).
	estimated := &manifest.Manifest{
		FinishedAt: now.Add(-3 * time.Hour),
		Tables: []core.TableInfo{
			{Name: "public.users", Rows: 2000},
			{Name: "public.orders", Rows: 5000},
		},
	}
	counted := &manifest.Manifest{
		FinishedAt: now.Add(-3 * time.Hour),
		Tables: []core.TableInfo{
			{Name: "public.users", Rows: 2000, RowsExact: true},
		},
	}

	tests := []struct {
		name      string
		manifest  *manifest.Manifest
		assertion Assertion
		inspector stubInspector
		wantOK    bool
		wantCount int
		want      string
	}{
		{
			name:      "table_count matches",
			manifest:  estimated,
			assertion: Assertion{Type: AssertTableCount},
			inspector: stubInspector{tables: []string{"public.users", "public.orders"}},
			wantOK:    true,
			wantCount: 1,
			want:      "2 tables, as the manifest records",
		},
		{
			name:      "table_count catches a table that did not come back",
			manifest:  estimated,
			assertion: Assertion{Type: AssertTableCount},
			inspector: stubInspector{tables: []string{"public.users"}},
			wantCount: 1,
			want:      "holds 1 tables, the manifest records 2",
		},
		{
			name:      "row_count checks every table by default",
			manifest:  estimated,
			assertion: Assertion{Type: AssertRowCount},
			inspector: stubInspector{rows: map[string]int64{"public.users": 2000, "public.orders": 5000}},
			wantOK:    true,
			wantCount: 2,
			want:      "public.users: 2000 rows against 2000 in the manifest",
		},
		{
			name:      "row_count narrows to the tables it names",
			manifest:  estimated,
			assertion: Assertion{Type: AssertRowCount, Tables: []string{"public.orders"}},
			inspector: stubInspector{rows: map[string]int64{"public.orders": 5000}},
			wantOK:    true,
			wantCount: 1,
			want:      "public.orders",
		},
		{
			name:      "row_count accepts a table named without its schema",
			manifest:  estimated,
			assertion: Assertion{Type: AssertRowCount, Tables: []string{"users"}},
			inspector: stubInspector{rows: map[string]int64{"public.users": 2000}},
			wantOK:    true,
			wantCount: 1,
			want:      "public.users",
		},
		{
			name:      "row_count reports a table the manifest never had",
			manifest:  estimated,
			assertion: Assertion{Type: AssertRowCount, Tables: []string{"public.invoices"}},
			wantCount: 1,
			want:      "records no table named public.invoices",
		},
		{
			// An estimate is a sample, and comparing it for equality would
			// measure the age of the statistics rather than the backup.
			name:      "row_count tolerates the drift of an estimate",
			manifest:  estimated,
			assertion: Assertion{Type: AssertRowCount, Tables: []string{"public.users"}},
			inspector: stubInspector{rows: map[string]int64{"public.users": 1900}},
			wantOK:    true,
			wantCount: 1,
			want:      "estimated, within 20%",
		},
		{
			name:      "row_count still catches a table that lost its rows",
			manifest:  estimated,
			assertion: Assertion{Type: AssertRowCount, Tables: []string{"public.users"}},
			inspector: stubInspector{rows: map[string]int64{"public.users": 0}},
			wantCount: 1,
			want:      "came back with 0 rows, the manifest records 2000",
		},
		{
			// A counted number is exact on both sides, so nothing is forgiven.
			name:      "row_count is exact when the manifest counted",
			manifest:  counted,
			assertion: Assertion{Type: AssertRowCount},
			inspector: stubInspector{rows: map[string]int64{"public.users": 1999}},
			wantCount: 1,
			want:      "came back with 1999 rows, the manifest records 2000",
		},
		{
			name:      "row_count honours the tolerance it was given",
			manifest:  counted,
			assertion: Assertion{Type: AssertRowCount, Tolerance: tolerance(0.05)},
			inspector: stubInspector{rows: map[string]int64{"public.users": 1950}},
			wantOK:    true,
			wantCount: 1,
			want:      "within 5%",
		},
		{
			name:      "row_count says so when the manifest has no counts",
			manifest:  &manifest.Manifest{Tables: []core.TableInfo{{Name: "public.users"}}},
			assertion: Assertion{Type: AssertRowCount},
			wantCount: 1,
			want:      "records no row counts to compare against",
		},
		{
			name:      "row_count reports a table it could not read",
			manifest:  counted,
			assertion: Assertion{Type: AssertRowCount},
			inspector: stubInspector{rowsErr: errors.New("relation does not exist")},
			wantCount: 1,
			want:      "the rows could not be counted",
		},
		{
			name:      "query compares the value it got",
			manifest:  counted,
			assertion: Assertion{Type: AssertQuery, SQL: "select count(*) from users where email is null", Expect: uint64(0)},
			inspector: stubInspector{value: int64(0)},
			wantOK:    true,
			wantCount: 1,
			want:      "returned 0, as expected",
		},
		{
			name:      "query fails on a different value",
			manifest:  counted,
			assertion: Assertion{Type: AssertQuery, SQL: "select count(*)\n  from users", Expect: 0},
			inspector: stubInspector{value: int64(3)},
			wantCount: 1,
			want:      "select count(*) from users returned 3, the assertion expects 0",
		},
		{
			name:      "query on an engine without one",
			manifest:  counted,
			assertion: Assertion{Type: AssertQuery, SQL: "select 1", Expect: 1},
			inspector: stubInspector{valueErr: core.ErrQueryUnsupported},
			wantCount: 1,
			want:      "no queries to assert on",
		},
		{
			name:      "max_age passes a fresh backup",
			manifest:  counted,
			assertion: Assertion{Type: AssertMaxAge, MaxAge: 26 * time.Hour},
			wantOK:    true,
			wantCount: 1,
			want:      "3h0m0s old, within 26h0m0s",
		},
		{
			name:      "max_age catches a backup that stopped being taken",
			manifest:  &manifest.Manifest{FinishedAt: now.Add(-72 * time.Hour)},
			assertion: Assertion{Type: AssertMaxAge, MaxAge: 26 * time.Hour},
			wantCount: 1,
			want:      "the backup is 72h0m0s old; the assertion allows 26h0m0s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			checks, err := runAssertions(context.Background(), tt.inspector, tt.manifest, []Assertion{tt.assertion}, now)

			require.NoError(t, err)
			require.Len(t, checks, tt.wantCount)

			var rendered strings.Builder
			passed := true
			for _, check := range checks {
				rendered.WriteString(check.Detail + "\n")
				passed = passed && check.OK
			}
			assert.Equal(t, tt.wantOK, passed, "checks: %s", rendered.String())
			assert.Contains(t, rendered.String(), tt.want)
		})
	}
}

// TestRunAssertionsStopsWhenCancelled: a cancelled run has found nothing, and
// reporting what it managed before the deadline as the verdict would be a lie.
func TestRunAssertionsStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	checks, err := runAssertions(ctx, stubInspector{}, &manifest.Manifest{}, []Assertion{{Type: AssertTableCount}}, time.Now())

	require.Error(t, err)
	assert.Empty(t, checks)
}

func TestValuesEqual(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want any
		same bool
	}{
		{name: "the decoders disagree about the type", got: int64(0), want: uint64(0), same: true},
		{name: "a float and an integer of the same value", got: float64(2), want: 2, same: true},
		{name: "mysql hands over bytes", got: []byte("7"), want: "7", same: true},
		{name: "different numbers", got: int64(3), want: 0},
		{name: "booleans", got: true, want: true, same: true},
		{name: "a bool against a number", got: true, want: 1},
		{name: "strings", got: "ok", want: "ok", same: true},
		{name: "null against zero", got: nil, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.same, valuesEqual(tt.got, tt.want))
		})
	}
}

func TestWithinTolerance(t *testing.T) {
	tests := []struct {
		name      string
		want      int64
		got       int64
		tolerance float64
		within    bool
	}{
		{name: "exact", want: 100, got: 100, within: true},
		{name: "off by one, no tolerance", want: 100, got: 101},
		{name: "off by one, within 5%", want: 100, got: 101, tolerance: 0.05, within: true},
		{name: "off by a third, within 5%", want: 300, got: 200, tolerance: 0.05},
		{name: "an empty table that should not be", want: 0, got: 500, tolerance: 0.2},
		{name: "a table that is empty in both", want: 0, got: 0, tolerance: 0.2, within: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.within, withinTolerance(tt.want, tt.got, tt.tolerance))
		})
	}
}
