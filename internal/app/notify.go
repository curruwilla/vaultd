package app

import (
	"fmt"
	"net/http"
	"time"

	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/notify"
)

// notifyTimeout bounds a whole delivery, retries included. It is deliberately
// short: a backup that has already finished should not be held open by a
// webhook receiver that stopped answering.
const notifyTimeout = 45 * time.Second

// Notifier builds the fanout of one target: the notifiers it names, each
// filtered to the events it subscribed to.
//
// It returns nil when the target notifies nobody, and every caller treats a
// nil notifier as "send nothing" — so a config with no notifiers block needs
// no special case anywhere else.
func (a *App) Notifier(target *config.Target) (core.Notifier, error) {
	if len(target.Notify) == 0 {
		return nil, nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if fanout, ok := a.fanouts[target.Name]; ok {
		return fanout, nil
	}

	subs := make([]notify.Subscription, 0, len(target.Notify))
	for _, name := range target.Notify {
		declared, ok := a.cfg.Notifier(name)
		if !ok {
			return nil, fmt.Errorf("target %q notifies %q, which is not declared under notifiers", target.Name, name)
		}

		notifier, err := a.notifierLocked(declared)
		if err != nil {
			return nil, err
		}

		sub := notify.Subscription{
			Name:     declared.Name,
			Notifier: notifier,
			Events:   declared.Events,
		}
		if declared.DedupWindow != nil {
			sub.Dedup = declared.DedupWindow.Duration()
		}
		subs = append(subs, sub)
	}

	fanout := notify.NewFanout(subs, a.log)
	a.fanouts[target.Name] = fanout
	return fanout, nil
}

// notifierLocked builds one notifier adapter, memoized by name: several
// targets usually share one webhook, and each carries an HTTP client.
//
// This switch is the notifier registry, the same shape as the engine and
// provider ones.
func (a *App) notifierLocked(declared *config.Notifier) (core.Notifier, error) {
	if existing, ok := a.notifiers[declared.Name]; ok {
		return existing, nil
	}

	var notifier core.Notifier
	switch declared.Type {
	case config.NotifierWebhook, "":
		notifier = &notify.Webhook{
			Name:     declared.Name,
			URL:      declared.URL,
			Template: notify.Template(declared.Template),
			Secret:   declared.Secret.Reveal(),
			Client:   a.httpClient(),
		}
	default:
		return nil, fmt.Errorf("notifier %q has unknown type %q", declared.Name, declared.Type)
	}

	a.notifiers[declared.Name] = notifier
	return notifier, nil
}

// httpClient is the client every webhook shares. Caller must hold a.mu.
func (a *App) httpClient() *http.Client {
	if a.client == nil {
		a.client = &http.Client{Timeout: notifyTimeout}
	}
	return a.client
}
