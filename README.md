# vaultd

Database backups to S3-compatible storage. One Go binary, one declarative config:
streaming dumps of MySQL/MariaDB, PostgreSQL and MongoDB, zstd compression, age
encryption, GFS retention and real restore verification.

**Status: feature complete for v0.1.** All four engines back up end to end —
PostgreSQL, MySQL, MariaDB and MongoDB — old backups expire on a GFS retention
policy, a stored backup can be checked and proven to restore, and `vaultd
serve` runs the whole thing on a schedule with signed webhooks, Prometheus
metrics and an embedded UI. Every command in SPEC §10 is implemented.

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

## Install

The container image is the primary distribution, because vaultd shells out to
the vendors' own dump clients and the image is where those are pinned.

```bash
# Every client, for a config that backs up more than one engine (~450MB).
docker pull ghcr.io/curruwilla/vaultd:latest

# One engine, for the common single-database case (~120MB).
docker pull ghcr.io/curruwilla/vaultd:latest-pg17
#   …-pg14 …-pg15 …-pg16 …-pg17 …-pg18 …-mysql8 …-mariadb11 …-mongo7
```

The same binary is in all of them. `vaultd doctor` reports which variant it is
running in, and a config naming an engine the image does not carry fails with
the image to use instead rather than with a missing-file error.

A standalone binary is the alternative, for a host that already has the
clients:

```bash
# Verify before you run it. The signature is bound to the workflow that built
# it, so there is no key to trust separately.
cosign verify-blob --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/curruwilla/vaultd/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
sha256sum -c checksums.txt --ignore-missing

tar xzf vaultd_*_linux_amd64.tar.gz
sudo install -m 0755 vaultd /usr/local/bin/
vaultd doctor          # says what is installed and what is missing
```

Ready-made deployments are in [`deploy/`](deploy/): a Compose stack, a
Kubernetes Deployment with the CronJob alternative beside it, and systemd units
for both modes.

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

| Command | What it does |
| --- | --- |
| `vaultd validate` | Parse, merge defaults, check everything that does not need a socket |
| `vaultd doctor [target...]` | The half that does: clients, databases, bucket, notifiers |
| `vaultd backup <target>` | Back one target up now, under the same lock the daemon takes |
| `vaultd list [target]` | The backups of a target, newest first |
| `vaultd verify [id]` | `--level integrity`, `structural` or `restore`; `--gc` collects leftovers |
| `vaultd restore <id> --to <dsn>` | Write a backup into an explicit destination, with `--confirm` |
| `vaultd prune <target>` | Apply the retention policy; dry run unless `--apply` |
| `vaultd reindex <target>` | Rebuild the listing cache from the manifests in the bucket |
| `vaultd serve` | The daemon: scheduler, locks, metrics, API, UI |
| `vaultd run` | Everything that is due, once, then exit |
| `vaultd version` | What this build is |

Exit codes are a contract: **0** ok, **1** failure, **2** usage.

## Running it as a daemon

`vaultd serve` is the supported way to run vaultd (decision D4). It evaluates
every target's schedule, takes that target's lock before running it, applies
the retention policy after a successful backup, and serves the HTTP endpoints.

```bash
vaultd serve                       # scheduler + metrics + API
vaultd run                         # one-shot: everything due, then exit
vaultd run --dry-run               # what is due, without running it
```

**The daemon keeps no state of its own.** What is due is derived from the cron
expression in the config and from when the target last ran, which the index in
the bucket records. A daemon that has just started, one that has been up for a
month, and a Kubernetes CronJob calling `vaultd run` all compute the same
answer — so a restart neither loses a schedule nor repeats one.

**Two replicas cannot run the same target at once.** Each takes a lease on
`_locks/<target>.lock` with a conditional create, renews it with a heartbeat,
and gives it up when the run ends; a holder that dies loses the lease when it
expires rather than blocking the target forever. The lock is shared with the
one-shot commands, so a manual `vaultd backup` and the daemon never collide.
The lock alone is not quite enough — a replica with a stale view would take the
lock the instant the other released it — so the due check runs again against
the index once the lock is held.

| Endpoint | Auth | What it is for |
| --- | --- | --- |
| `/healthz` | open | liveness — the process is up. Never depends on the bucket |
| `/readyz` | open | readiness — the config is valid and the destinations answer |
| `/metrics` | token | Prometheus |
| `/api/…` | token | the API the UI is built on |
| `/` | open shell | the UI — it carries no data, and every byte it shows comes from `/api` |

The probes are open on purpose: a liveness check that needs a secret fails when
the secret is rotated. Everything else is behind `server.auth.token`, as a
bearer header or a cookie, with failed attempts throttled per source address.
Prometheus reads it with `authorization: { credentials_file: … }`.

`on_overlap` decides what happens when a run is still going at its next slot:
`skip` (default), `queue` (run it once the current one finishes) or `fail`.
Either way the event is `schedule.missed`.

## The UI

`server.ui: true` serves a single-page app from the same address, compiled into
the binary — nothing to mount, nothing to keep in step with a deployment, and
nothing loaded from a CDN.

- **Overview** — a card per target with a traffic light. Red is a rule, not a
  feeling: the most recent run failed, or the newest backup is past its
  `max_age` (the `max_age` assertion if one is declared, otherwise twice the
  schedule's own interval). Amber is a verification that failed, or a backup
  nothing has verified yet. Every colour carries its reason.
- **Target** — the timeline of runs, failures included, with what the next
  prune would keep and delete. *Back up now* and *Verify now* run through the
  same executor the schedule uses, so they take the same lock and land in the
  same index, with the run's own log streamed back beside the button.
- **Backup** — the manifest, the verification result and its assertions, a
  *Download* that hands back a five-minute presigned URL (the daemon never
  proxies the bytes and the browser never sees a bucket credential), and
  *Copy restore command*.
- **Config** — the effective configuration re-rendered as YAML with every
  secret replaced by `***`, plus `doctor` on demand.

Pruning from the UI requires the dry run first, and applying sends back a
digest of the exact plan that was shown: if a backup finished in between, the
digest no longer matches and the new plan is shown instead.

**Restoring into production is not in the UI** (SPEC §13). It is a CLI command
with `--confirm`, and the UI's contribution is the command to paste.

## Notifications and metrics

`vaultd validate` proves the config is coherent without opening a socket.
`vaultd doctor` proves the world it describes exists: which database clients
are installed, whether every target's server answers, whether each bucket
accepts the two conditional writes the target lock and the index are built on
(`If-None-Match`, `If-Match`) — with a canary object it deletes again — and
whether the notifier endpoints are reachable.

```bash
vaultd doctor                 # everything
vaultd doctor prod-pg         # one target
vaultd doctor --json          # for a monitoring check
vaultd doctor --notify        # also send a real signed test delivery
```

Notifiers are dialled, not posted to, unless `--notify` is given: a notifier
subscribed to `backup.failed` usually points at somebody's pager, and a health
check that pages on-call every time it runs is a health check people mute.

Events are delivered as signed JSON — `X-Vaultd-Signature: sha256=<hmac>` and
`X-Vaultd-Event` — in the generic vaultd shape, or rendered natively for Slack
and Discord (`template:`). Delivery is three attempts with jittered backoff,
and a `4xx` is not retried: the receiver understood and refused. **A webhook
that is down never fails a backup** — the backup is already in the bucket.

`vaultd serve` exposes Prometheus metrics. The one worth alerting on is
`vaultd_backup_last_success_timestamp`: every other series describes a run that
happened, and a backup tool fails by runs not happening at all.

| Metric | Type |
| --- | --- |
| `vaultd_backup_last_success_timestamp{target}` | gauge — alert on its age |
| `vaultd_backup_duration_seconds{target,engine}` | histogram |
| `vaultd_backup_bytes{target,kind}` | gauge — `compressed` and `plain` |
| `vaultd_backup_failures_total{target,phase}` | counter |
| `vaultd_verify_last_success_timestamp{target,level}` | gauge |
| `vaultd_retention_objects{target,tier}` | gauge |

## Releases

A tag builds everything: static binaries for linux and darwin on amd64 and
arm64, the nine image variants for `linux/amd64` and `linux/arm64`, an SBOM per
artifact, and keyless cosign signatures over the checksums and the images. The
images also carry a build-provenance attestation, so what produced them is
verifiable without trusting the registry.

The release workflow re-runs the gates against the tag itself — build, vet,
race tests, `validate-example`, `govulncheck` — because a release is the one
build nobody re-runs, and then smoke-tests the published images. Locally:

```bash
make release-check           # everything the workflow checks, before tagging
make snapshot                # build the artifacts without publishing
make docker ENGINE=pg17      # one image variant
make docker-smoke            # …and check it reports its variant and clients
```

Three things are part of the contract and will not change without a major
version: the exit codes (0 ok, 1 failure, 2 usage), the manifest schema, and
the object key layout in the bucket. See [CHANGELOG.md](CHANGELOG.md).

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
internal/notify/     webhook delivery: signing, retry, dedup, chat templates
internal/lock/       the per-target lease in the bucket: acquire, heartbeat, release
internal/scheduler/  what is due, and the one execution path all modes share
internal/prune/      applying a retention plan: objects first, index second
internal/server/     HTTP: probes, metrics, the API, the embedded UI
internal/metrics/    the Prometheus collectors
internal/doctor/     the network half of the config check
internal/app/        config to adapters, the wiring layer
internal/cli/        the cobra command tree
internal/buildinfo/  version stamped at link time
internal/logging/    slog setup
test/e2e/            acceptance tests against real containers and real R2
web/                 the single-page UI, embedded with go:embed
examples/            example config and demo environment
deploy/              Compose, Kubernetes and systemd, ready to adapt
```

Adapters live next to the port they implement: an engine under
`internal/engine/`, a bucket under `internal/storage/`, a webhook under
`internal/notify/`. `internal/core/` imports none of them.
