package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/notify"
)

// received is one delivery a test server captured.
type received struct {
	body      []byte
	signature string
	event     string
}

// recorder is a webhook receiver that answers with a scripted sequence of
// status codes and remembers everything it was sent.
type recorder struct {
	statuses []int
	calls    atomic.Int32
	got      []received
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.got = append(r.got, received{
		body:      body,
		signature: req.Header.Get(notify.SignatureHeader),
		event:     req.Header.Get(notify.EventHeader),
	})

	call := int(r.calls.Add(1)) - 1
	status := http.StatusOK
	if call < len(r.statuses) {
		status = r.statuses[call]
	}
	w.WriteHeader(status)
}

func notification() core.Notification {
	return notify.Notification(core.EventBackupFailed,
		time.Date(2026, 8, 24, 3, 15, 0, 0, time.UTC), "prod-pg", "prod-pg backup failed")
}

// A receiver must be able to verify what it read, which means the signature is
// over the exact bytes on the wire — not over a re-encoding of them.
func TestSignatureCoversTheDeliveredBytes(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	server := httptest.NewServer(rec)
	defer server.Close()

	webhook := &notify.Webhook{Name: "ops", URL: server.URL, Secret: "s3cret"}
	require.NoError(t, webhook.Notify(context.Background(), notification()))

	require.Len(t, rec.got, 1)
	got := rec.got[0]

	assert.Equal(t, "backup.failed", got.event)
	assert.True(t, notify.Verify("s3cret", got.body, got.signature),
		"the signature must verify against the bytes the receiver read")
	assert.False(t, notify.Verify("wrong", got.body, got.signature))
}

func TestUnsignedWhenNoSecret(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	server := httptest.NewServer(rec)
	defer server.Close()

	webhook := &notify.Webhook{Name: "ops", URL: server.URL}
	require.NoError(t, webhook.Notify(context.Background(), notification()))

	require.Len(t, rec.got, 1)
	assert.Empty(t, rec.got[0].signature)
}

func TestRetriesTransientFailures(t *testing.T) {
	t.Parallel()

	rec := &recorder{statuses: []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusOK}}
	server := httptest.NewServer(rec)
	defer server.Close()

	webhook := &notify.Webhook{
		Name:  "ops",
		URL:   server.URL,
		Sleep: func(context.Context, time.Duration) {},
	}

	require.NoError(t, webhook.Notify(context.Background(), notification()))
	assert.Equal(t, int32(3), rec.calls.Load())
}

// A 4xx is the receiver saying it understood and refused. Repeating it only
// makes the same complaint three times.
func TestDoesNotRetryARejection(t *testing.T) {
	t.Parallel()

	rec := &recorder{statuses: []int{http.StatusBadRequest}}
	server := httptest.NewServer(rec)
	defer server.Close()

	webhook := &notify.Webhook{
		Name:  "ops",
		URL:   server.URL,
		Sleep: func(context.Context, time.Duration) {},
	}

	err := webhook.Notify(context.Background(), notification())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `notifier "ops"`)
	assert.Equal(t, int32(1), rec.calls.Load())
}

func TestGivesUpAfterTheAttemptBudget(t *testing.T) {
	t.Parallel()

	rec := &recorder{statuses: []int{500, 500, 500, 500}}
	server := httptest.NewServer(rec)
	defer server.Close()

	webhook := &notify.Webhook{
		Name:  "ops",
		URL:   server.URL,
		Sleep: func(context.Context, time.Duration) {},
	}

	require.Error(t, webhook.Notify(context.Background(), notification()))
	assert.Equal(t, int32(3), rec.calls.Load(), "three attempts, as SPEC §12 says")
}

func TestGenericPayloadIsTheDocumentedShape(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	server := httptest.NewServer(rec)
	defer server.Close()

	n := notification()
	n.BackupID = "01JMX"
	n.DurationMS = 12043
	n.Error = &core.Failure{Phase: "dump", Code: "DUMP_EXIT_1", Message: "boom", StderrTail: "pg_dump: error"}

	webhook := &notify.Webhook{Name: "ops", URL: server.URL, Template: notify.TemplateGeneric}
	require.NoError(t, webhook.Notify(context.Background(), n))

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.got[0].body, &got))

	assert.Equal(t, "backup.failed", got["event"])
	assert.Equal(t, "prod-pg", got["target"])
	assert.Equal(t, "01JMX", got["backup_id"])
	assert.Equal(t, "critical", got["severity"], "severity comes from the event, never from the config")
	assert.InDelta(t, float64(12043), got["duration_ms"], 0)

	failure, ok := got["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "dump", failure["phase"])
	assert.Equal(t, "DUMP_EXIT_1", failure["code"])
	assert.Equal(t, "pg_dump: error", failure["stderr_tail"])
}

func TestChatTemplatesRenderTheirNativeShape(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		template notify.Template
		rich     string
		plain    string
	}{
		{notify.TemplateSlack, "attachments", "text"},
		{notify.TemplateDiscord, "embeds", "content"},
	} {
		t.Run(string(tc.template), func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			server := httptest.NewServer(rec)
			defer server.Close()

			webhook := &notify.Webhook{Name: "chat", URL: server.URL, Template: tc.template}
			require.NoError(t, webhook.Notify(context.Background(), notification()))

			var got map[string]any
			require.NoError(t, json.Unmarshal(rec.got[0].body, &got))

			assert.Contains(t, got, tc.rich)
			assert.Contains(t, got, tc.plain, "a client that renders nothing still shows the summary")
		})
	}
}

func TestUnknownTemplateIsRefusedBeforeAnyRequest(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	server := httptest.NewServer(rec)
	defer server.Close()

	webhook := &notify.Webhook{Name: "ops", URL: server.URL, Template: "teams"}

	err := webhook.Notify(context.Background(), notification())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown template")
	assert.Zero(t, rec.calls.Load())
}

// The stderr tail is 64KB in a manifest; a chat message that long is useless.
func TestChatTemplatesTrimTheStderrTail(t *testing.T) {
	t.Parallel()

	n := notification()
	n.Error = &core.Failure{Message: "boom", StderrTail: strings.Repeat("line\n", 200)}

	body, err := notify.TemplateSlack.Render(n)
	require.NoError(t, err)
	assert.Less(t, len(body), 2000)
}
