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
- Go 1.24 is the floor (`go.mod`). `CGO_ENABLED=0` everywhere.
- `make lint` runs golangci-lint under a pinned toolchain: 2.12 panics analyzing
  a Go 1.27 standard library.

## Before committing

```bash
make fmt lint test
make validate-example   # the M0 acceptance gate
```
