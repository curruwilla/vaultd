# vaultd

Database backups to S3-compatible storage. One Go binary, one declarative config:
streaming dumps of MySQL/MariaDB, PostgreSQL and MongoDB, zstd compression, age
encryption, GFS retention and real restore verification.

**Status: M2.** All four engines back up end to end — PostgreSQL, MySQL,
MariaDB and MongoDB. `vaultd backup` streams a dump through compression and age
encryption into an S3-compatible bucket and writes a manifest beside it, and
`vaultd list` reads those manifests back. Retention, verification and the daemon
follow. Commands that are not implemented yet are registered, document their
flags, and fail with the milestone that brings them.

| Engine | Client it drives | Consistency it achieves |
| --- | --- | --- |
| PostgreSQL | `pg_dump -Fc` (never older than the server) | deferrable serializable snapshot |
| MySQL | `mysqldump` | single transaction (InnoDB), or every table locked |
| MariaDB | `mariadb-dump` | single transaction (InnoDB), or every table locked |
| MongoDB | `mongodump --archive` | point in time on a replica set, per collection otherwise |

Each engine is probed before it is dumped, and the flags follow what the probe
found: no replication position when the user lacks `RELOAD` and
`REPLICATION CLIENT`, no oplog on a standalone MongoDB, no GTID purge on
MariaDB. Passing a flag the server or the user cannot support makes the client
abort partway through a dump, so the probe decides instead.

## Quickstart

```bash
make build

# Validate the shipped example with demo values (no network, no credentials).
make validate-example

# Validate your own config.
export VAULTD_AGE_RECIPIENT=age1... PROD_PG_DSN=postgres://...
./bin/vaultd validate -c vaultd.yaml

# Back one target up, then look at what landed in the bucket.
./bin/vaultd backup prod-pg --dry-run    # probe and plan, write nothing
./bin/vaultd backup prod-pg
./bin/vaultd list prod-pg
```

A backup is one pass with nothing buffered on disk:

```
pg_dump -Fc ──► zstd ──► age ──► adaptive multipart ──► bucket
      │                                                    │
      └──── sha256 of the plaintext ──► manifest ◄─── sha256 of the object
```

Both checksums go into the manifest because they answer different questions:
the object checksum proves the bucket holds what was uploaded, the plaintext
checksum proves a restore reproduced what the database handed over.

Database clients are not bundled: vaultd shells out to the vendor's own, and
says exactly what is missing (`server is PG 17, need pg_dump >= 17, found
16.4`). A PostgreSQL client older than the server is refused outright — its
archive format cannot represent everything the server holds. MySQL and MariaDB
emit portable SQL, so an older client there is a warning, but dumping MariaDB
with Oracle's client is refused: the flags and the output diverge.

Credentials never reach a client through its command line, which is
world-readable on Linux: PostgreSQL and MySQL take them from the environment,
and MongoDB — which has no such variable — reads its URI from an inherited pipe.

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
| `vaultd backup <target>` | ready — PostgreSQL, MySQL, MariaDB, MongoDB |
| `vaultd list [target]` | ready |
| `vaultd prune`, `vaultd reindex` | M3 |
| `vaultd verify`, `vaultd restore` | M4 |
| `vaultd doctor` | M6 |
| `vaultd serve`, `vaultd run` | M7 |

## Development

```bash
make test              # unit tests
make test-race         # under the race detector
make cover             # coverage.html
make fuzz              # fuzz the config parser for 60s
make lint              # golangci-lint
make test-integration  # container-backed tests (needs Docker)
make test-e2e          # the end-to-end acceptance suite
make help              # every target
```

The integration suite starts PostgreSQL, MySQL, MariaDB, MongoDB and MinIO with
testcontainers and runs real backups through them. It needs each engine's client
on the host. Without root, unpack them all:

```bash
make dev-clients   # unpacks clients under .cache/ and prints the exports
export PATH=... LD_LIBRARY_PATH=... VAULTD_TEST_PG_IMAGE=postgres:16-alpine
make test-integration
```

Tests that need a client skip themselves when it is absent, so `make
test-integration` stays useful on a bare machine.

The same acceptance test runs against a real Cloudflare R2 bucket when these
are set (nightly in CI, skipped otherwise):

```
VAULTD_E2E_R2_ENDPOINT   VAULTD_E2E_R2_BUCKET
VAULTD_E2E_R2_ACCESS_KEY_ID   VAULTD_E2E_R2_SECRET_ACCESS_KEY
```

Go 1.24 or newer. `make lint` pins its own toolchain: golangci-lint 2.12 cannot
analyze a Go 1.27 standard library yet.

### Layout

```
cmd/vaultd/          entry point, nothing but wiring
internal/core/       ports: Dumper, Restorer, Store — the interfaces everything is written against
internal/config/     parse, interpolate, merge defaults, validate, redact
internal/pipeline/   compress → encrypt → hash, over io.Pipe
internal/engine/     client resolution and stderr capture
  postgres/          pg_dump, pg_dumpall and the catalog probe
  mysql/             mysqldump and mariadb-dump, and the flags each server accepts
  mongodb/           mongodump, replica-set detection, credentials over a pipe
internal/storage/    s3/ (AWS, R2, MinIO) and memory/ for tests
internal/manifest/   manifest schema, object key layout, index
internal/backup/     the orchestration: probe → stream → manifest
internal/app/        config to adapters, the wiring layer
internal/cli/        the cobra command tree
internal/buildinfo/  version stamped at link time
internal/logging/    slog setup
test/e2e/            acceptance tests against real containers and real R2
examples/            example config and demo environment
```

Adapters land next to their port as they arrive: `internal/notify/` in M6.
