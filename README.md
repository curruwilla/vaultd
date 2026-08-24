# vaultd

Database backups to S3-compatible storage. One Go binary, one declarative config:
streaming dumps of MySQL/MariaDB, PostgreSQL and MongoDB, zstd compression, age
encryption, GFS retention and real restore verification.

**Status: M0.** The config layer, `vaultd validate`, the CLI surface and CI are in
place. Every other command is registered and documents its flags, but fails with
the milestone that will implement it.

## Quickstart

```bash
make build

# Validate the shipped example with demo values (no network, no credentials).
make validate-example

# Validate your own config.
export VAULTD_AGE_RECIPIENT=age1... PROD_PG_DSN=postgres://...
./bin/vaultd validate -c vaultd.yaml
```

`validate` parses the config, expands its `${VAR}` and `${file:/run/secrets/x}`
references, applies the defaults every target inherits, and runs every semantic
check — cross-references, cron syntax, age recipients, per-engine options,
retention sanity. It never opens a socket; connectivity checks belong to
`vaultd doctor`.

Useful flags:

| Flag | Effect |
| --- | --- |
| `--json` | Machine-readable diagnostics, with a summary and severity counts |
| `--allow-unset-env` | Unresolved `${VAR}` becomes a warning — validating a config where the secrets are absent |
| `--print-effective` | Print the merged config with every secret redacted |

## Configuration

See [`examples/config.yaml`](examples/config.yaml). Secrets never live in the
file: `${VAR}` reads the environment and `${file:/path}` reads a mounted secret.

Encryption is mandatory to declare. A target with no `encryption` block — its own
or inherited — fails validation:

```
error: target "prod-pg" has no encryption; set encryption.recipients or opt out with encryption.mode=none
```

Opting out is legal, but it is a line in the YAML that a reviewer can see.

## Commands

| Command | Status |
| --- | --- |
| `vaultd validate` | ready |
| `vaultd version` | ready |
| `vaultd backup <target>`, `vaultd list` | M1 |
| `vaultd prune`, `vaultd reindex` | M3 |
| `vaultd verify`, `vaultd restore` | M4 |
| `vaultd doctor` | M6 |
| `vaultd serve`, `vaultd run` | M7 |

## Development

```bash
make test        # go test ./...
make test-race   # under the race detector
make cover       # coverage.html
make fuzz        # fuzz the config parser for 60s
make lint        # golangci-lint
make help        # every target
```

Go 1.24 or newer. `make lint` pins its own toolchain: golangci-lint 2.12 cannot
analyze a Go 1.27 standard library yet.

### Layout

```
cmd/vaultd/         entry point, nothing but wiring
internal/core/      ports: Dumper, Restorer, Store — the interfaces everything is written against
internal/config/    parse, interpolate, merge defaults, validate, redact
internal/cli/       the cobra command tree
internal/buildinfo/ version stamped at link time
internal/logging/   slog setup
examples/           example config and demo environment
```

Adapters land next to their port as they arrive: `internal/engine/` (postgres,
mysql, mongodb), `internal/storage/` (s3, r2, minio), `internal/notify/`.
