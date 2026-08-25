package index

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/curruwilla/vaultd/internal/manifest"
)

var pruneAt = time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)

func kept(id string, when time.Time) manifest.Entry {
	return manifest.Entry{ID: id, Target: "prod-pg", Outcome: manifest.OutcomeSucceeded, FinishedAt: when, Key: id + ".dump"}
}

func failure(when time.Time) manifest.Entry {
	return manifest.NewFailureEntry("prod-pg", when, when, "dump", "pg_dump exited with code 1")
}

// The index after a prune must describe exactly what the bucket still holds,
// plus the failure records that still fall inside the retained window.
func TestPruneKeepsSurvivorsAndRecentFailures(t *testing.T) {
	entries := []manifest.Entry{
		kept("01A", pruneAt),                   // deleted
		failure(pruneAt.Add(30 * time.Minute)), // predates the window
		kept("01B", pruneAt.Add(time.Hour)),    // survives
		failure(pruneAt.Add(90 * time.Minute)), // inside the window
		kept("01C", pruneAt.Add(2*time.Hour)),  // survives
	}

	left := prune(entries, map[string]bool{"01A": true}, pruneAt.Add(time.Hour))

	ids := make([]string, 0, len(left))
	for _, entry := range left {
		if entry.Succeeded() {
			ids = append(ids, entry.ID)
			continue
		}
		ids = append(ids, "failed@"+entry.FinishedAt.Format(time.TimeOnly))
	}

	assert.Equal(t, []string{"01B", "failed@04:30:00", "01C"}, ids)
}

func TestPruneKeepsEveryFailureWhenNothingSurvives(t *testing.T) {
	entries := []manifest.Entry{kept("01A", pruneAt), failure(pruneAt.Add(time.Hour))}

	left := prune(entries, map[string]bool{"01A": true}, time.Time{})

	assert.Len(t, left, 1)
	assert.False(t, left[0].Succeeded())
}

func TestPruneKeepsUnlistedBackups(t *testing.T) {
	entries := []manifest.Entry{kept("01A", pruneAt), kept("01B", pruneAt.Add(time.Hour))}

	// A backup that finished while prune was running is not in the deleted
	// set, so it stays.
	left := prune(entries, map[string]bool{"01A": true}, pruneAt.Add(time.Hour))

	assert.Len(t, left, 1)
	assert.Equal(t, "01B", left[0].ID)
}
