package verify

import (
	"fmt"
	"strings"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/notify"
)

// verifiedEvent describes a verification that ran to a conclusion.
//
// A failure here is critical and says so: the backup it describes is the one
// somebody would have reached for, and finding out it does not come back is
// worth waking a person for (SPEC §8).
func verifiedEvent(m *manifest.Manifest, result Result) core.Notification {
	event := core.EventVerifySucceeded
	summary := fmt.Sprintf("%s passed %s verification of the backup from %s",
		m.Target, result.Level, m.FinishedAt.Format("2006-01-02 15:04Z"))

	if !result.OK {
		event = core.EventVerifyFailed
		summary = fmt.Sprintf("%s failed %s verification of the backup from %s: %s",
			m.Target, result.Level, m.FinishedAt.Format("2006-01-02 15:04Z"), firstProblem(result))
	}

	n := notify.Notification(event, result.At, m.Target, summary)
	n.BackupID = m.ID
	n.DurationMS = result.Duration.Milliseconds()
	n.Details = map[string]any{
		"level":  string(result.Level),
		"object": m.Object.Key,
	}
	if len(result.Problems) > 0 {
		n.Details["problems"] = result.Problems
	}
	if checks, ok := result.Details["assertions"].([]Check); ok && len(checks) > 0 {
		n.Details["assertions"] = summarizeChecks(checks)
	}

	if !result.OK {
		n.Error = &core.Failure{
			Phase:   string(result.Level),
			Code:    "VERIFY_" + strings.ToUpper(string(result.Level)) + "_FAILED",
			Message: firstProblem(result),
		}
	}
	return n
}

// firstProblem is what goes in the one-line summary. The rest travel in the
// details: a chat message that opens with all six problems gets collapsed.
func firstProblem(result Result) string {
	if len(result.Problems) == 0 {
		return "the check reported no detail"
	}
	return result.Problems[0]
}

// summarizeChecks renders the assertions as short strings, so a receiver that
// only shows text still says which assertion failed and on what.
func summarizeChecks(checks []Check) []string {
	out := make([]string, 0, len(checks))
	for _, check := range checks {
		mark := "ok"
		if !check.OK {
			mark = "FAIL"
		}
		out = append(out, fmt.Sprintf("%s %s: %s", mark, check.Type, check.Detail))
	}
	return out
}
