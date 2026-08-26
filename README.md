# vaultd

Database backups to S3-compatible storage. One Go binary, one declarative config:
streaming dumps of MySQL/MariaDB, PostgreSQL and MongoDB, zstd compression, age
encryption, GFS retention and real restore verification.

**Status: M5.** All four engines back up end to end — PostgreSQL, MySQL,
MariaDB and MongoDB — old backups expire on a GFS retention policy, and a
stored backup can be checked, restored, and proven to restore. `vaultd backup`
streams a dump through compression and age encryption into an S3-compatible
bucket and writes a manifest beside it; `vaultd list` reads the index back;
`vaultd prune` applies the policy; `vaultd verify` proves a backup still reads
back — and, at level `restore`, that it comes back into a real server with the
rows it claims; `vaultd restore` writes one into a database. Notifiers, metrics
and the daemon follow. Commands that are not implemented yet are registered,
document their flags, and fail with the milestone that brings them.

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

## Retention

Retention is grandfather-father-son. A backup survives if it represents a
period some tier still keeps, and one backup can represent several at once —
the Sunday backup is usually the daily, the weekly, and on the first of the
month the monthly one too:

```yaml
retention:
  hourly:  { keep: 24 }
  daily:   { keep: 7  }
  weekly:  { keep: 4, on: sunday }
  monthly: { keep: 12, on: 1 }
  yearly:  { keep: 3 }
  min_keep: 3          # the floor, whatever the rules above say
```

`keep: 7` means the seven most recent days **that hold a backup**, not the last
seven calendar days: a week of downtime does not expire everything behind it.
Which tiers a backup belongs to is worked out from the timestamps every time,
so a policy change applies to the backups that already exist.

`vaultd prune` reports and changes nothing unless `--apply` is given, and
refuses to delete at all when:

- it would leave fewer than `min_keep` backups;
- it would remove the most recent **verified** backup — the only one anything
  has evidence restores;
- the most recent run of that target **failed**. A fresh backup that just broke
  is exactly when the old ones matter.

Objects that no manifest refers to — the residue of an interrupted run — are
never touched by a normal prune. `--orphans` reports them, and only removes
them together with `--apply`, ignoring anything written in the last 24 hours so
a run that is still uploading is safe.

```console
$ vaultd prune prod-pg
ACTION  FINISHED           SIZE     REASON               ID
keep    2026-08-24 03:00Z  1.2 GB   daily+weekly         01JMX…
keep    2026-08-17 03:00Z  1.2 GB   weekly               01JHW…
delete  2026-08-23 03:00Z  1.2 GB   —                    01JMA…

dry run: 1 backup (1.2 GB) would be deleted; re-run with --apply to carry it out
```

The per-target index (`<prefix>/_index/<target>.jsonl`) is a listing cache, not
a second source of truth: it records every run, successful or failed, and
`vaultd reindex` rebuilds it from the manifests whenever it is lost or stale.

## Verification and restore

A backup nobody has read back is a hypothesis. `vaultd verify` turns it into a
fact, at three costs:

| Level | What it does | Cost |
| --- | --- | --- |
| `integrity` | one HEAD per object: it is there, and it is the size the manifest records | no egress, safe to run after every backup |
| `structural` | reads the object back in full, decrypts it, decompresses it, checks the plaintext checksum against the manifest and the format against the engine | egress and CPU, worth a daily schedule |
| `restore` | restores the backup into a database of its own on a staging server, runs the configured assertions against it, and drops it | egress, CPU and a server, worth a weekly schedule |

```console
$ vaultd verify --target prod-pg --latest --level structural --identity-file key.age
ok: prod-pg passed structural verification
  backup     01JMX… (2026-08-24 03:00Z)
  level      structural in 12.4s
  read back  19.4 GB
  format     a pg_dump custom-format archive
```

A flipped byte fails it — age authenticates what it decrypts, so corruption
surfaces before a checksum comparison is even reached. The outcome is written
onto the manifest and into the index, which is what stops `prune` from deleting
the most recent verified backup.

### Restore verification

`--level restore` is the only check that proves a backup comes back rather than
that its bytes are intact. It needs a `verify_targets` entry: a staging server
whose credential may create databases inside one prefix, and nothing else.

```yaml
targets:
  - name: prod-pg
    # …
    verify:
      level: restore
      schedule: "0 5 * * 0"
      into: staging-pg
      assertions:
        - type: table_count               # as many tables as the manifest records
        - type: row_count                 # each table's rows, against the manifest
          tables: ["public.users", "public.orders"]
        - type: query                     # your own SQL, against the restored data
          sql: "select count(*) from users where email is null"
          expect: 0
        - type: max_age                   # the backup is newer than this
          value: 26h

verify_targets:
  - name: staging-pg
    engine: postgres
    dsn: ${STAGING_PG_ADMIN_DSN}          # needs CREATEDB and nothing beyond it
    database_prefix: vaultd_verify_       # the only names vaultd creates or drops
    max_concurrent: 1
```

```console
$ vaultd verify --target prod-pg --latest --level restore --identity-file key.age
ok: prod-pg passed restore verification
  backup         01JMX… (2026-08-24 03:00Z)
  level          restore in 4m12s
  restored into  vaultd_verify_01jmx…
  read back      19.4 GB
  assertions
    ok  table_count  38 tables, as the manifest records
    ok  row_count    public.users: 1928372 rows against 1928372 in the manifest (estimated, within 20%)
    ok  query        select count(*) from users where email is null returned 0, as expected
    ok  max_age      the backup is 2h14m0s old, within 26h0m0s
```

- The database is named after the backup and dropped afterwards, whatever the
  outcome. `vaultd verify --gc` collects what a crashed run left behind.
- Row counts in a manifest are estimates by default (`row_estimate: estimate`),
  so a `row_count` assertion compares within a band rather than demanding
  equality — an exact comparison would measure the age of the server's
  statistics. Set `row_estimate: exact`, or give the assertion a `tolerance`,
  to decide that yourself.
- A backup that will not restore is a **finding**, not an error: it is reported,
  recorded on the manifest and in the index, and the command exits non-zero.
- A staging server older than the source is a **skip**, with the reason: the
  backup is not what is wrong, so nothing is recorded and a good verification
  from last week survives.
- MongoDB archives carry the database they came from, so a restore into an
  ephemeral one is a namespace rename; a backup holding several databases is
  skipped with that reason rather than restored over the staging server's own
  names.

Reading a backup back needs the age private key, and vaultd never stores it:
it lives away from the machine that takes the backups (a compromised backup
host that cannot read its own backups is the feature). Pass it as a file —
`--identity-file`, or `VAULTD_AGE_IDENTITY_FILE` — never as a flag value, since
argv is world-readable.

Restore is always explicit about where it writes:

```console
$ vaultd restore 01JMX… --to postgres://…/restored --confirm --identity-file key.age
```

- `--confirm` is required: a restore writes to a live database.
- A destination that already holds data is refused unless `--force`.
- A `--to` that matches a database this config backs up is refused unless
  `--force`, because restoring over production is rarely what was meant.
- The plaintext is checksummed as it streams into the client and compared with
  the manifest, so a restore that applied cleanly but wrote different bytes
  than were backed up is reported as the failure it is.

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
| `vaultd prune <target>` | ready — dry run unless `--apply` |
| `vaultd reindex <target>` | ready |
| `vaultd verify [id]` | ready — integrity, structural and restore (`--gc` collects leftovers) |
| `vaultd restore <id> --to <dsn>` | ready |
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
internal/core/       ports: Dumper, Restorer, Store, Provisioner — the interfaces everything is written against
internal/config/     parse, interpolate, merge defaults, validate, redact
internal/pipeline/   compress → encrypt → hash, over io.Pipe
internal/engine/     client resolution and stderr capture
  postgres/          pg_dump, pg_dumpall, the catalog probe and the verify sandbox
  mysql/             mysqldump and mariadb-dump, and the flags each server accepts
  mongodb/           mongodump, replica-set detection, credentials over a pipe
internal/storage/    s3/ (AWS, R2, MinIO) and memory/ for tests
internal/manifest/   manifest schema, object key layout, index entries
internal/index/      the listing cache: conditional append, rebuild
internal/retention/  GFS classification and the invariants prune obeys
internal/verify/     L0/L1/L2 checks, the format validators and the assertions
internal/restore/    streaming a stored backup back into a database
internal/backup/     the orchestration: probe → stream → manifest
internal/app/        config to adapters, the wiring layer
internal/cli/        the cobra command tree
internal/buildinfo/  version stamped at link time
internal/logging/    slog setup
test/e2e/            acceptance tests against real containers and real R2
examples/            example config and demo environment
```

Adapters land next to their port as they arrive: `internal/notify/` in M6.
