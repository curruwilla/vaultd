package doctor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
)

// canaryDir is where the probe object lives. It is a sibling of _index and
// _locks, so a least-privilege policy scoped to the prefix already covers it.
const canaryDir = "_doctor"

// checkDestinations proves the bucket accepts every operation vaultd depends
// on, with one throwaway object.
//
// The conditional writes are the part worth checking. Everything else is a
// plain S3 API that every implementation has; If-None-Match and If-Match are
// newer, and a store without them silently breaks the target lock and the
// append-only index — a failure that would otherwise surface as two daemons
// dumping the same database at once.
func (d *Doctor) checkDestinations(ctx context.Context, report *Report, targets []*config.Target) {
	used := map[string]bool{}
	for _, target := range targets {
		used[target.Destination] = true
	}

	for _, name := range sortedNames(used) {
		dest, ok := d.App.Config().Destination(name)
		if !ok {
			report.add(Check{
				Group:  "destinations",
				Name:   name,
				Status: StatusFail,
				Detail: "not declared",
			})
			continue
		}

		for _, check := range d.checkDestination(ctx, dest) {
			report.add(check)
		}
	}
}

func (d *Doctor) checkDestination(ctx context.Context, dest *config.Destination) []Check {
	store, err := d.App.Store(ctx, dest.Name)
	if err != nil {
		return []Check{{
			Group:  "destinations",
			Name:   dest.Name,
			Status: StatusFail,
			Detail: err.Error(),
		}}
	}

	key := canaryKey(dest.Prefix)
	body := []byte("vaultd doctor canary\n")

	checks := []Check{{
		Group:  "destinations",
		Name:   dest.Name,
		Status: StatusOK,
		Detail: fmt.Sprintf("%s bucket %s, probing %s", dest.Provider, dest.Bucket, key),
	}}

	// Whatever happens below, the canary goes away: a bucket left holding
	// doctor's litter is a bad first impression and a real (tiny) cost.
	defer func() {
		if err := store.Delete(context.WithoutCancel(ctx), []string{key}); err != nil {
			d.log().WarnContext(ctx, "the doctor canary was not removed", "key", key, "error", err)
		}
	}()

	created := d.withTimeout(ctx, func(ctx context.Context) Check {
		return conditionalPut(ctx, store, dest.Name, key, body)
	})
	checks = append(checks, created)
	if created.Status == StatusFail {
		return checks
	}

	checks = append(checks,
		d.withTimeout(ctx, func(ctx context.Context) Check {
			return readBack(ctx, store, dest.Name, key, body)
		}),
		d.withTimeout(ctx, func(ctx context.Context) Check {
			return overwriteIfMatch(ctx, store, dest.Name, key, body)
		}),
		d.withTimeout(ctx, func(ctx context.Context) Check {
			return deleteCanary(ctx, store, dest.Name, key)
		}),
	)
	return checks
}

// conditionalPut is the lock primitive: PutIfAbsent must create the object,
// and must refuse the second attempt.
func conditionalPut(ctx context.Context, store core.Store, dest, key string, body []byte) Check {
	check := Check{Group: "destinations", Name: dest + ": conditional write"}

	created, err := store.PutIfAbsent(ctx, key, body)
	switch {
	case err != nil:
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("PutObject with If-None-Match failed: %s", err)
		check.Hint = "vaultd locks a target with a conditional write; a store without one cannot stop two runs colliding"
		return check
	case !created:
		check.Status = StatusFail
		check.Detail = "the canary key already exists"
		check.Hint = "delete " + key + " and run doctor again"
		return check
	}

	// The same call must now lose: a store that always reports "created" would
	// hand the lock to every caller at once.
	again, err := store.PutIfAbsent(ctx, key, body)
	switch {
	case err != nil:
		check.Status = StatusWarn
		check.Detail = fmt.Sprintf("the second conditional write errored instead of refusing: %s", err)
	case again:
		check.Status = StatusFail
		check.Detail = "If-None-Match is not honoured: the same key was created twice"
		check.Hint = "this bucket cannot hold the target lock; two daemons would dump the same database at once"
	default:
		check.Status = StatusOK
		check.Detail = "created, and the second attempt was refused"
	}
	return check
}

// readBack proves Head and Get agree with what was written.
func readBack(ctx context.Context, store core.Store, dest, key string, body []byte) Check {
	check := Check{Group: "destinations", Name: dest + ": read back"}

	info, err := store.Head(ctx, key)
	if err != nil {
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("HeadObject failed: %s", err)
		return check
	}
	if info.Bytes != int64(len(body)) {
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("HeadObject reports %d bytes for a %d byte object", info.Bytes, len(body))
		return check
	}

	reader, err := store.Get(ctx, key)
	if err != nil {
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("GetObject failed: %s", err)
		return check
	}
	defer reader.Close()

	got, err := io.ReadAll(reader)
	if err != nil {
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("reading the object back failed: %s", err)
		return check
	}
	if !bytes.Equal(got, body) {
		check.Status = StatusFail
		check.Detail = "the object read back does not match what was written"
		return check
	}

	check.Status = StatusOK
	check.Detail = fmt.Sprintf("%d bytes, etag %s", info.Bytes, shorten(info.ETag))
	return check
}

// overwriteIfMatch is the index primitive: a write conditioned on the current
// etag must succeed, and the same write with a stale etag must be refused.
func overwriteIfMatch(ctx context.Context, store core.Store, dest, key string, body []byte) Check {
	check := Check{Group: "destinations", Name: dest + ": compare-and-swap"}

	info, err := store.Head(ctx, key)
	if err != nil {
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("HeadObject failed: %s", err)
		return check
	}

	updated, written, err := store.PutIfMatch(ctx, key, append(body, 'x'), info.ETag)
	switch {
	case err != nil:
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("PutObject with If-Match failed: %s", err)
		check.Hint = "the append-only index needs compare-and-swap; without it a concurrent writer truncates entries"
		return check
	case !written:
		check.Status = StatusFail
		check.Detail = "a write conditioned on the current etag was refused"
		return check
	}

	// The stale etag is the interesting half: it is what makes a concurrent
	// index writer retry instead of overwriting entries it never read.
	_, written, err = store.PutIfMatch(ctx, key, body, info.ETag)
	switch {
	case err != nil:
		check.Status = StatusWarn
		check.Detail = fmt.Sprintf("a stale conditional write errored instead of being refused: %s", err)
	case written:
		check.Status = StatusFail
		check.Detail = "If-Match is not honoured: a write with a stale etag went through"
		check.Hint = "concurrent index updates would silently lose entries on this bucket"
	default:
		check.Status = StatusOK
		check.Detail = fmt.Sprintf("swapped at etag %s, and a stale etag was refused", shorten(updated.ETag))
	}
	return check
}

func deleteCanary(ctx context.Context, store core.Store, dest, key string) Check {
	check := Check{Group: "destinations", Name: dest + ": delete"}

	if err := store.Delete(ctx, []string{key}); err != nil {
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("DeleteObject failed: %s", err)
		check.Hint = "prune needs DeleteObject on this prefix"
		return check
	}

	if _, err := store.Head(ctx, key); !errors.Is(err, core.ErrNotFound) {
		check.Status = StatusWarn
		check.Detail = "the object still answers HeadObject after a delete (eventual consistency?)"
		return check
	}

	check.Status = StatusOK
	check.Detail = "the canary is gone"
	return check
}

// canaryKey is unique per run so two doctors on two hosts do not collide.
func canaryKey(prefix string) string {
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])

	name := "canary-" + hex.EncodeToString(suffix[:])
	if prefix == "" {
		return path.Join(canaryDir, name)
	}
	return path.Join(strings.Trim(prefix, "/"), canaryDir, name)
}

func shorten(s string) string {
	s = strings.Trim(s, `"`)
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "…"
}
