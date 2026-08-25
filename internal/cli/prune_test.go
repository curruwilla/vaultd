package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/manifest"
)

var pruneAt = time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)

func succeeded(id string, when time.Time) manifest.Entry {
	return manifest.Entry{
		ID:          id,
		Target:      "prod-pg",
		Outcome:     manifest.OutcomeSucceeded,
		FinishedAt:  when,
		Key:         id + ".pgdump.zst.age",
		ManifestKey: id + ".manifest.json",
		Bytes:       2048,
	}
}

func failed(when time.Time) manifest.Entry {
	return manifest.NewFailureEntry("prod-pg", when, when, "dump", "pg_dump exited with code 1")
}

func TestBackupsOfSkipsFailures(t *testing.T) {
	entries := []manifest.Entry{
		succeeded("01A", pruneAt),
		failed(pruneAt.Add(time.Hour)),
		succeeded("01B", pruneAt.Add(2*time.Hour)),
	}

	backups := backupsOf(entries)

	require.Len(t, backups, 2)
	assert.Equal(t, "01A", backups[0].ID)
	// Every object of the backup travels with it, because that is what a
	// deletion has to remove.
	assert.Equal(t, []string{"01A.pgdump.zst.age", "01A.manifest.json"}, backups[0].Keys)
}

func TestBackupsOfCarriesVerification(t *testing.T) {
	verified := succeeded("01A", pruneAt)
	ok := true
	verified.VerifyOK = &ok

	backups := backupsOf([]manifest.Entry{verified})

	require.Len(t, backups, 1)
	assert.True(t, backups[0].Verified)
}

func TestLastRunFailed(t *testing.T) {
	tests := []struct {
		name    string
		entries []manifest.Entry
		want    bool
	}{
		{
			name:    "no runs at all",
			entries: nil,
		},
		{
			name:    "the newest run succeeded",
			entries: []manifest.Entry{failed(pruneAt), succeeded("01B", pruneAt.Add(time.Hour))},
		},
		{
			name:    "the newest run failed",
			entries: []manifest.Entry{succeeded("01A", pruneAt), failed(pruneAt.Add(time.Hour))},
			want:    true,
		},
		{
			name:    "order in the file does not matter, only time does",
			entries: []manifest.Entry{failed(pruneAt.Add(time.Hour)), succeeded("01A", pruneAt)},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lastRunFailed(tt.entries))
		})
	}
}
