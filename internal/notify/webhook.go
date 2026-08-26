// Package notify delivers events to the endpoints the config declares
// (SPEC §12).
//
// Two rules shape everything here. A webhook that is down must never fail a
// backup — the backup is already in the bucket, and turning a delivery problem
// into a backup problem would make the tool less trustworthy, not more. And a
// payload is signed with the bytes that actually go on the wire, so a receiver
// can verify what it read rather than what we meant to send.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
)

// Headers a receiver reads. They are part of the contract: a receiver matches
// on them, so they may not be renamed casually.
const (
	SignatureHeader = "X-Vaultd-Signature"
	EventHeader     = "X-Vaultd-Event"
)

const (
	// defaultAttempts is the delivery budget of one event (SPEC §12).
	defaultAttempts = 3
	// defaultTimeout bounds a single POST. A receiver that needs longer than
	// this to acknowledge a webhook is doing work it should have queued.
	defaultTimeout = 10 * time.Second
	// baseBackoff is the first retry delay; it doubles per attempt.
	baseBackoff = time.Second
	// bodyLimit is how much of a failing response is quoted back in the error.
	bodyLimit = 512
)

// Webhook posts notifications to one URL.
type Webhook struct {
	// Name is the notifier's name in the config, so a delivery failure names
	// the block the operator has to look at.
	Name     string
	URL      string
	Template Template
	// Secret keys the HMAC signature. Empty means unsigned, which the config
	// warns about rather than refuses.
	Secret string

	Client   *http.Client
	Attempts int
	// Sleep is the retry delay; tests replace it so a backoff costs no time.
	Sleep func(context.Context, time.Duration)
}

// Notify delivers one notification, retrying only what is worth retrying.
func (w *Webhook) Notify(ctx context.Context, n core.Notification) error {
	body, err := w.Template.Render(n)
	if err != nil {
		return fmt.Errorf("notifier %q: %w", w.Name, err)
	}

	attempts := w.Attempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}

	var last error
	for attempt := range attempts {
		if attempt > 0 {
			w.sleep(ctx, backoff(attempt))
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("notifier %q: %w", w.Name, errors.Join(last, err))
			}
		}

		err := w.post(ctx, n.Event, body)
		if err == nil {
			return nil
		}
		last = err

		// A 4xx means the receiver understood the request and rejected it.
		// Sending it twice more only makes the same complaint three times.
		var permanent *permanentError
		if errors.As(err, &permanent) {
			break
		}
	}

	return fmt.Errorf("notifier %q: %w", w.Name, last)
}

// post is one delivery attempt.
func (w *Webhook) post(ctx context.Context, event core.Event, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return &permanentError{err: err}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "vaultd")
	req.Header.Set(EventHeader, string(event))
	if w.Secret != "" {
		req.Header.Set(SignatureHeader, Sign(w.Secret, body))
	}

	resp, err := w.client().Do(req)
	if err != nil {
		// A connection that never completed is exactly what a retry is for.
		return fmt.Errorf("posting to the webhook: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return fmt.Errorf("the webhook answered %s", statusOf(resp))
	default:
		return &permanentError{err: fmt.Errorf("the webhook answered %s", statusOf(resp))}
	}
}

// Sign returns the value of the signature header for a body: the receiver
// recomputes it over the raw bytes it read and compares in constant time.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether signature matches body under secret. vaultd does not
// receive its own webhooks; this exists so the tests — and anyone writing a
// receiver — check the signature the way it is meant to be checked.
func Verify(secret string, body []byte, signature string) bool {
	return hmac.Equal([]byte(Sign(secret, body)), []byte(signature))
}

// permanentError marks a failure no retry can fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func (w *Webhook) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return http.DefaultClient
}

func (w *Webhook) sleep(ctx context.Context, d time.Duration) {
	if w.Sleep != nil {
		w.Sleep(ctx, d)
		return
	}

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// backoff is exponential with jitter. The jitter matters when a daemon holding
// several targets reacts to one outage: without it every notifier retries in
// lockstep and hits the recovering receiver together.
func backoff(attempt int) time.Duration {
	d := baseBackoff << (attempt - 1)
	jitter, err := rand.Int(rand.Reader, big.NewInt(int64(d/2)))
	if err != nil {
		return d
	}
	return d/2 + time.Duration(jitter.Int64())
}

// statusOf renders a failing response for an error message, quoting the start
// of the body: receivers usually explain themselves there.
func statusOf(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if len(bytes.TrimSpace(body)) == 0 {
		return resp.Status
	}
	return fmt.Sprintf("%s: %s", resp.Status, bytes.TrimSpace(body))
}
