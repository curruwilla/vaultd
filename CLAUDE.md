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

- `internal/core/` holds the ports — `Dumper`, `Restorer`, `Store` — plus the
  value types they exchange. It imports nothing from the rest of the project.
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
make test-integration    # the M1 acceptance gate (needs Docker and a pg_dump)
```
