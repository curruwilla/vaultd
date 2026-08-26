package doctor

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/notify"
)

// checkNotifiers checks the endpoints events are delivered to.
//
// By default it only proves the endpoint is there: DNS resolves, the port
// accepts a connection, and TLS completes. It does not POST anything, because
// a notifier subscribed to backup.failed usually points at somebody's pager,
// and a health check that pages on-call every time it runs is a health check
// people mute. `--notify` sends the real signed delivery when that is what the
// operator wants to test.
func (d *Doctor) checkNotifiers(ctx context.Context, report *Report, cfg *config.Config) {
	for i := range cfg.Notifiers {
		declared := &cfg.Notifiers[i]

		report.add(d.withTimeout(ctx, func(ctx context.Context) Check {
			if d.Notify {
				return d.deliverTest(ctx, declared)
			}
			return reachable(ctx, declared)
		}))
	}
}

// reachable dials the endpoint without sending anything.
func reachable(ctx context.Context, declared *config.Notifier) Check {
	check := Check{Group: "notifiers", Name: declared.Name}

	u, err := url.Parse(declared.URL)
	if err != nil || u.Host == "" {
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("%q is not a URL that can be dialled", declared.URL)
		return check
	}

	address := u.Host
	if u.Port() == "" {
		address = net.JoinHostPort(u.Hostname(), portFor(u.Scheme))
	}

	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("cannot reach %s: %s", address, err)
		return check
	}
	defer conn.Close()

	if u.Scheme == "https" {
		// A certificate this host does not trust is the failure that would
		// otherwise only show up on the night of the first real incident.
		tlsConn := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		defer tlsConn.Close()

		if err := tlsConn.HandshakeContext(ctx); err != nil {
			check.Status = StatusFail
			check.Detail = fmt.Sprintf("TLS handshake with %s failed: %s", u.Hostname(), err)
			return check
		}
	}

	check.Status = StatusOK
	check.Detail = address + " answers; pass --notify to send a signed test delivery"
	if !declared.Secret.Set() {
		check.Status = StatusWarn
		check.Hint = "no secret is set, so deliveries go out unsigned and a receiver cannot tell them from anyone else's"
	}
	return check
}

// deliverTest posts a real, signed notification.
//
// It borrows backup.started rather than inventing a doctor-only event, so a
// receiver that filters on the events it subscribed to still sees it — a test
// delivery that gets dropped by the receiver's own routing has tested nothing.
func (d *Doctor) deliverTest(ctx context.Context, declared *config.Notifier) Check {
	check := Check{Group: "notifiers", Name: declared.Name}

	event := core.EventBackupStarted
	if len(declared.Events) > 0 {
		event = declared.Events[0]
	}

	webhook := &notify.Webhook{
		Name:     declared.Name,
		URL:      declared.URL,
		Template: notify.Template(declared.Template),
		Secret:   declared.Secret.Reveal(),
		Attempts: 1,
	}

	n := notify.Notification(event, time.Now().UTC(), "",
		fmt.Sprintf("vaultd doctor test delivery for notifier %q — no backup ran", declared.Name))
	n.Details = map[string]any{"test": true, "template": string(declared.Template)}

	if err := webhook.Notify(ctx, n); err != nil {
		check.Status = StatusFail
		check.Detail = err.Error()
		return check
	}

	check.Status = StatusOK
	check.Detail = fmt.Sprintf("a signed %s payload was accepted (sent as %s)", declared.Template, event)
	if !declared.Secret.Set() {
		check.Status = StatusWarn
		check.Detail = fmt.Sprintf("an unsigned %s payload was accepted (sent as %s)", declared.Template, event)
		check.Hint = "set a secret so the receiver can verify the signature"
	}
	return check
}

func portFor(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}
