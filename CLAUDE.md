# vaultd — agent instructions

Backup tool for MySQL/MariaDB, PostgreSQL and MongoDB to S3-compatible storage.
`SPEC.md` (untracked, local) is the source of truth for scope, decisions D1–D7
and the M0–M9 milestone plan. Read it before proposing design changes.

## Required Go skills

Always load `samber/cc-skills-golang@golang-how-to` before any Go work in this
repository. It routes to the specific Go skills a task needs (testing, error
handling, concurrency, CLI, security, lint).

## Architecture

Hexagonal, ports and adapters:

- `internal/core/` holds the ports — `Dumper`, `Restorer`, `Store`, and
  `Provisioner`/`Sandbox`/`Inspector` for the ephemeral databases restore
  verification uses — plus the value types they exchange. It imports nothing
  from the rest of the project.
- Adapters implement a port and live beside it: `internal/engine/{postgres,
  mysql,mongodb}`, `internal/storage/s3`, `internal/notify/`.
- Pipeline, retention and verify logic stays pure and testable without a
  network: they take ports, never concrete clients.
- Dependency injection is manual constructor wiring, plus a registry keyed by
  engine or provider name for what the config picks at runtime. No DI framework.
- `cmd/vaultd/main.go` only calls into `internal/cli`.

## How a backup flows

`cli/backup.go` → `app` (config to adapters) → `backup.Runner` → `pipeline.Run`,
which drives two goroutines over an `io.Pipe`: the dumper writes, the store
reads. Whichever side fails first closes the pipe with its error, so the other
side fails fast instead of deadlocking, and `backup.Error` records which phase
died (`probe`, `dump`, `upload`, `manifest`).

Invariants worth keeping:

- **Nothing is buffered whole.** Memory is bounded by the compressor window and
  the uploader's in-flight parts (`pipeline.zstdLongWindow`,
  `s3.maxPartSize` × `s3.maxConcurrency`). Raising either raises RSS; the target
  is under 256MB for a database of any size.
- **A manifest is a claim that a restorable backup exists.** It is written last,
  and never for a failed run.
- **A failed upload leaves nothing behind.** The multipart upload is aborted
  even when the context is already cancelled.
- **Both checksums are recorded**: the object's (integrity in the bucket) and
  the plaintext's (what a restore must reproduce).
- **Clients are resolved by version, not by PATH order.** `pg_dump` must be at
  least the server's major; anything else fails before the dump starts.
- **Connection details reach a client through the environment, never argv** —
  argv is world-readable.

## Engine adapters

Every engine shells out to the vendor's client and probes the server first. The
probe is not a formality: it decides the command line.

- **Never pass a flag the server or the user cannot support.** A client that
  rejects a flag aborts partway through the dump, after it has already written
  part of the stream. `--source-data` needs binary logging plus RELOAD and
  REPLICATION CLIENT; `--set-gtid-purged` needs GTIDs on; `--oplog` needs a
  replica set and a full-deployment dump. The probe checks, the flag follows.
- **Degrade loudly, fail early.** Something the operator asked for but the
  server cannot give (an oplog on a standalone) becomes a warning on the
  manifest. Something that would fail mid-dump (locking without RELOAD) is
  refused before the dump starts.
- **The manifest never overstates consistency.** A MySQL database with MyISAM
  tables dumped under `--single-transaction` is `best_effort`, not
  `single_transaction`.
- **Credentials never reach argv.** PostgreSQL and MySQL take them from the
  environment; MongoDB has no such variable, so its URI goes through a pipe the
  child inherits (`--config=/dev/fd/3`).
- **Client output is redacted before it is stored.** mongodump and the drivers
  echo the connection string, password included, into their errors — and those
  errors travel into manifests and webhooks.
- **Version rules differ by engine, on purpose.** A PostgreSQL client older than
  its server is refused (the archive format cannot represent everything); a
  MySQL or MariaDB one only warns (portable SQL); a client from the wrong fork
  is always refused.

## Retention and the index

- **Tiers are computed, not stamped.** `retention.Policy.Plan` classifies
  backups from their timestamps every time it runs, so a policy change applies
  retroactively and no recorded tier can drift out of step with the config. The
  manifest's `tier` field is the label recorded at creation, nothing more.
- **Periods are calendar components, never elapsed time.** `bucketKey` formats
  a local date; nothing divides by 24 hours. That is what makes a 23- or
  25-hour day, a leap day and an ISO week across a year boundary behave.
- **A gap does not consume a slot.** `keep: 7` is the seven most recent periods
  that hold a backup.
- **The invariants in SPEC §7 are load-bearing.** Each has a test that fails
  loudly if the rule is dropped: the `min_keep` floor, the most recent verified
  backup, and the freeze after a failed run. An empty policy keeps everything —
  the floor must never turn "no policy" into "delete all but min_keep".
- **The index is a cache.** The manifests in the bucket are the truth.
  Appending is a conditional write (`PutIfMatch`), so a concurrent writer loses
  the race and retries instead of truncating entries it never saw, and a failed
  index update never fails a stored backup.
- **Failures are indexed too.** Without them, retention cannot tell a quiet
  week from a broken one, and invariant 3 would have nothing to read.
- **Delete objects first, then rewrite the index.** A stale index that still
  lists a deleted backup is a nuisance; one that hides a backup which is still
  there hides a restore.

## Verification and restore

- **A broken backup is a finding, not an error.** Corruption, a missing object
  or a size that disagrees with the manifest come back as `Result.Problems`
  with `OK: false`. Errors are reserved for what stops the check from running
  at all — an unreachable bucket, or a key that matches no recipient. Reporting
  a good backup as broken because the operator passed the wrong key would be
  worse than saying nothing.
- **Verification results are written back** to the manifest and the index. That
  is the only reason retention can protect the most recent verified backup.
- **The private key is never stored and never on argv.** It arrives as a file
  path (`--identity-file`, `VAULTD_AGE_IDENTITY_FILE`) for the run that needs
  it, which is also why `structural` and `restore` are the only commands that
  ask for one.
- **Restore says where it writes, every time.** `--confirm` is mandatory, a
  non-empty destination needs `--force`, and a `--to` that matches a configured
  target needs `--force` as well.
- **The restored stream is checksummed on the way past.** A client that exits 0
  having consumed half the archive leaves a database holding half a backup;
  comparing against the manifest is what catches it.
- **L2 restores into a database of its own, and drops it.** The name is the
  verify target's `database_prefix` plus the backup id, the drop is deferred on
  a context that survives cancellation, and `verify --gc` collects what a
  crashed process could not. A `Provisioner` refuses every name outside the
  prefix, which is what stands between a verification and a staging database
  somebody cares about.
- **A skip is not a failure, and is not recorded.** A staging server older than
  the source, or a topology that cannot be renamed into one ephemeral database,
  comes back as `Result.Skipped` with a reason. Writing it down would replace
  the verification a backup already earned with the absence of one; exiting
  non-zero would break a nightly run over something the backup did not do.
- **An estimate is compared as an estimate.** Manifest row counts are the
  planner's by default (D7), so a `row_count` assertion with no `tolerance`
  compares within `estimateTolerance` and says so in its detail. An explicit
  `tolerance: 0` means exact, because the operator asked; `validate` warns when
  that meets `row_estimate: estimate`.
- **Assertions collect, they do not stop** — the same rule as `config.Validate`,
  and each check records what it compared, not merely that it failed.

## Conventions

- **Secrets never print.** Connection strings, tokens and keys are
  `config.Secret`, which redacts itself through `String`, `GoString`,
  `MarshalJSON`, `MarshalYAML` and `slog.LogValuer`. Use `.Reveal()` only where
  the value is handed to the process that needs it; `config.RedactDSN` renders a
  DSN safely for logs and errors.
- **Validation collects, it does not stop.** `config.Validate` appends
  `Diagnostic` values so one run reports every problem. Messages name the
  offending target and say what to do:
  `target "prod-pg" has no encryption; set encryption.recipients or opt out with encryption.mode=none`.
- **`validate` never touches the network.** Anything that opens a socket belongs
  in `doctor`.
- **The config decoder is strict**: unknown or duplicated keys are errors. A typo
  that silently does nothing is the failure mode a backup tool cannot afford.
- Exit codes are a contract: 0 ok, 1 failure, 2 usage.
- Go 1.24 is the floor (`go.mod`). `CGO_ENABLED=0` everywhere; every dependency
  is pure Go, including the PostgreSQL driver (pgx) and the SQLite planned for
  the UI cache.
- Container-backed tests live behind the `integration` build tag and skip
  themselves when a database client is missing, so `go test ./...` stays fast
  and green on any machine.
- `make lint` runs golangci-lint under a pinned toolchain: 2.12 panics analyzing
  a Go 1.27 standard library.

## Before committing

```bash
make fmt lint test
make validate-example    # the M0 acceptance gate
make test-integration    # the M1–M5 acceptance gates (needs Docker and the clients)
```
