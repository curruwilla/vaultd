# Changelog

Notable changes, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

Three things are part of the contract and will not change without a major
version: the **exit codes** (0 ok, 1 failure, 2 usage), the **manifest schema**
(readers refuse a schema they do not understand rather than guess), and the
**object key layout** in the bucket.

## [Unreleased]

## [0.1.0]

The first release. Backs up MySQL, MariaDB, PostgreSQL and MongoDB to
S3-compatible storage, expires old backups on a GFS policy, and proves a backup
restores rather than assuming it.

### Added

- **Backups.** `vaultd backup` streams a dump through zstd and age into the
  bucket in one pass, with nothing buffered on disk, and writes a manifest
  beside it recording both checksums — the object's and the plaintext's.
  Memory is bounded by the compressor window and the uploader's in-flight
  parts rather than by the size of the database.
- **Four engines**, each probed before it is dumped so the flags follow what
  the server can actually give: no replication position without `RELOAD`, no
  oplog on a standalone, no GTID purge on MariaDB. Clients are resolved by
  version, and a PostgreSQL client older than its server is refused outright.
- **Retention.** `vaultd prune` classifies backups into GFS tiers from their
  timestamps every time it runs, so a policy change applies retroactively.
  Dry run by default; the `min_keep` floor, the most recent verified backup and
  the freeze after a failed run are enforced and tested.
- **Verification.** `vaultd verify` at three levels: the objects are there
  (`integrity`), they read back byte for byte (`structural`), or they restore
  into an ephemeral database on a staging server and satisfy the configured
  assertions (`restore`). A broken backup is a finding, not an error.
- **Restore.** `vaultd restore` writes a backup into an explicit destination,
  checksumming the stream on the way past, refusing a non-empty database
  without `--force` and requiring `--confirm` every time.
- **Notifications and metrics.** Signed webhooks (generic, Slack, Discord) with
  retry and per-target deduplication, and the Prometheus series an age alert
  needs. A webhook that is down never fails a backup.
- **`vaultd doctor`.** The half of the config check that needs the network:
  which clients are installed and which fork they belong to, whether every
  database answers, and whether each bucket honours the conditional writes the
  lock and the index depend on.
- **The daemon.** `vaultd serve` runs the schedule, takes a lease on each
  target before running it, and serves `/healthz`, `/readyz`, `/metrics`, the
  API and the UI. It keeps no state of its own, so a restart neither loses a
  schedule nor repeats one, and two replicas cannot run the same target at
  once. `vaultd run` is the same thing once, for a CronJob or a timer.
- **The UI.** Four screens compiled into the binary: the overview with an
  explainable traffic light, the target timeline with its projected retention,
  the backup with its manifest and a presigned download, and the effective
  config with every secret redacted.
- **Distribution.** Multi-arch container images — one fat, one per engine —
  and static binaries with checksums, SBOMs and keyless signatures.

### Decisions

Recorded in `SPEC.md` §20 and unlikely to be revisited:

- **D1** — a fat image plus one slim image per engine, same binary in all.
- **D2** — retention is applied by vaultd, not by a bucket lifecycle rule:
  lifecycle only knows age and prefix, and cannot honour the invariants.
- **D3** — restore verification runs against a declared staging server, not a
  Docker socket, so it works the same on a VM, bare metal and Kubernetes.
- **D4** — the daemon is the primary way to run vaultd; the one-shot commands
  remain first-class and share its lock.
- **D5** — encryption is on by default and must be declared. A config with no
  `encryption` block fails validation; opting out is a line in the YAML that a
  reviewer can see.
- **D6** — vaultd is a backup tool, and not an umbrella for other operations.
- **D7** — manifest row counts are the planner's estimates by default, so a
  `row_count` assertion with no `tolerance` compares within a band and says so.

[Unreleased]: https://github.com/curruwilla/vaultd/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/curruwilla/vaultd/releases/tag/v0.1.0
