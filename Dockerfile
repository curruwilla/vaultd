# syntax=docker/dockerfile:1.7
#
# vaultd images (SPEC §3, decision D1).
#
# "One binary" does not mean "no dependencies": vaultd shells out to the
# vendors' own dump clients, because a pure-Go dumper cannot match pg_dump or
# mysqldump on extensions, generated columns, triggers, charsets or GTID state
# — and a backup that restores incorrectly is worse than no backup.
#
# So the image is the primary distribution, and it comes in two shapes:
#
#   ENGINE=all         every client, ~450MB, for a multi-engine config
#   ENGINE=pg17 …      one engine, ~120MB, for the common single-database case
#
# The same binary is in all of them. VAULTD_VARIANT tells `vaultd doctor` which
# one it is running in, so a config that needs mysqldump in a pg17 image gets a
# clear error rather than a mysterious one.
#
#   docker build --build-arg ENGINE=pg17 -t vaultd:pg17 .

ARG ENGINE=all
ARG GO_VERSION=1.25
ARG DEBIAN=bookworm

# --- build ------------------------------------------------------------------

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-${DEBIAN} AS build

WORKDIR /src

# Dependencies first: they change far less often than the code, and this is the
# layer worth caching.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# CGO off everywhere: every dependency is pure Go, including the PostgreSQL
# driver and the SQLite the UI cache will use, so the binary is static and runs
# on any base image — including scratch, for anyone who brings their own
# clients.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/curruwilla/vaultd/internal/buildinfo.Version=${VERSION} \
        -X github.com/curruwilla/vaultd/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/curruwilla/vaultd/internal/buildinfo.Date=${DATE}" \
      -o /out/vaultd ./cmd/vaultd

# --- client layers ----------------------------------------------------------

FROM debian:${DEBIAN}-slim AS base

# ca-certificates is not optional: every destination is HTTPS, and a container
# without them fails at the first PUT with an error that reads like a
# credential problem. curl and gnupg are build-time only and are purged by the
# stages that use them — the final image should not carry a downloader.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*

# repos adds the three vendor apt repositories.
#
# PGDG is what makes several PostgreSQL client majors installable side by side,
# which is the whole reason vaultd can pick one by the server's version rather
# than by PATH order (SPEC §3).
#
# Oracle's and MariaDB's are both needed because Debian ships one client for
# both forks — `default-mysql-client` is MariaDB's — and vaultd refuses a
# client from the wrong fork: the flags and the output diverge, so dumping
# MySQL with mariadb-dump is not a near-enough answer. Debian's MariaDB is also
# 10.11 rather than the 11.x LTS the SPEC pins.
FROM base AS repos
# Oracle rotates its apt signing key and publishes the replacement under a new
# year. Every one that is still published is imported, newest first, so a build
# keeps working across a rotation — and so an expired key does not become a
# release that cannot be built.
ARG MYSQL_KEYS="RPM-GPG-KEY-mysql-2025 RPM-GPG-KEY-mysql-2023"
ARG MARIADB_SERIES=11.4
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends curl gnupg; \
    codename="$(. /etc/os-release && echo "$VERSION_CODENAME")"; \
    install -d /usr/share/keyrings; \
    curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
      -o /usr/share/keyrings/pgdg.asc; \
    echo "deb [signed-by=/usr/share/keyrings/pgdg.asc] https://apt.postgresql.org/pub/repos/apt ${codename}-pgdg main" \
      > /etc/apt/sources.list.d/pgdg.list; \
    for key in ${MYSQL_KEYS}; do \
      curl -fsSL "https://repo.mysql.com/${key}" >> /tmp/mysql-keys.asc || true; \
    done; \
    gpg --dearmor -o /usr/share/keyrings/mysql.gpg < /tmp/mysql-keys.asc; \
    rm -f /tmp/mysql-keys.asc; \
    echo "deb [signed-by=/usr/share/keyrings/mysql.gpg] https://repo.mysql.com/apt/debian ${codename} mysql-8.0" \
      > /etc/apt/sources.list.d/mysql.list; \
    curl -fsSL https://mariadb.org/mariadb_release_signing_key.pgp \
      -o /usr/share/keyrings/mariadb.pgp; \
    echo "deb [signed-by=/usr/share/keyrings/mariadb.pgp] https://mirror.mariadb.org/repo/${MARIADB_SERIES}/debian ${codename} main" \
      > /etc/apt/sources.list.d/mariadb.list; \
    rm -rf /var/lib/apt/lists/*

# tools downloads the MongoDB database tools, which are not in any distribution
# archive. They are versioned independently of the server (100.x) and a current
# one handles every supported server, so there is exactly one to fetch.
FROM base AS tools
ARG TARGETARCH
ARG MONGO_TOOLS_VERSION=100.10.0
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends curl; \
    case "${TARGETARCH}" in \
      amd64) arch=x86_64 ;; \
      arm64) arch=arm64 ;; \
      *) echo "no MongoDB tools build for ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/mongodb-database-tools.deb \
      "https://fastdl.mongodb.org/tools/db/mongodb-database-tools-debian12-${arch}-${MONGO_TOOLS_VERSION}.deb"; \
    rm -rf /var/lib/apt/lists/*

FROM repos AS engine-pg14
RUN apt-get update && apt-get install -y --no-install-recommends postgresql-client-14 && rm -rf /var/lib/apt/lists/*

FROM repos AS engine-pg15
RUN apt-get update && apt-get install -y --no-install-recommends postgresql-client-15 && rm -rf /var/lib/apt/lists/*

FROM repos AS engine-pg16
RUN apt-get update && apt-get install -y --no-install-recommends postgresql-client-16 && rm -rf /var/lib/apt/lists/*

FROM repos AS engine-pg17
RUN apt-get update && apt-get install -y --no-install-recommends postgresql-client-17 && rm -rf /var/lib/apt/lists/*

FROM repos AS engine-pg18
RUN apt-get update && apt-get install -y --no-install-recommends postgresql-client-18 && rm -rf /var/lib/apt/lists/*

# The dump client and the interactive one: a restore feeds the dump back
# through mysql/mariadb, so shipping only the dumper would make the image able
# to back up and not to restore.
FROM repos AS engine-mysql8
RUN apt-get update && apt-get install -y --no-install-recommends mysql-community-client && rm -rf /var/lib/apt/lists/*

FROM repos AS engine-mariadb11
RUN apt-get update && apt-get install -y --no-install-recommends mariadb-client && rm -rf /var/lib/apt/lists/*

FROM base AS engine-mongo7
COPY --from=tools /tmp/mongodb-database-tools.deb /tmp/
RUN apt-get update \
 && apt-get install -y --no-install-recommends /tmp/mongodb-database-tools.deb \
 && rm -rf /tmp/mongodb-database-tools.deb /var/lib/apt/lists/*

# The fat image: every engine, for a config that backs up more than one.
#
# The two MySQL forks cannot both be installed: each declares Conflicts on the
# other's virtual-mysql-client. So Oracle's goes in normally — it has the more
# involved dependencies — and MariaDB's is unpacked beside it, then linked into
# /usr/local/bin, which Debian puts ahead of /usr/bin on PATH.
#
# The result is what vaultd's resolution expects: `mysqldump` is Oracle's,
# `mariadb-dump` is MariaDB's, and neither pretends to be the other. Getting
# this wrong would not fail the build — it would produce an image whose
# `mysqldump` is a MariaDB symlink, which vaultd then refuses at dump time.
FROM repos AS engine-all
COPY --from=tools /tmp/mongodb-database-tools.deb /tmp/
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      postgresql-client-14 postgresql-client-15 postgresql-client-16 \
      postgresql-client-17 postgresql-client-18 \
      mysql-community-client libmariadb3 \
      libedit2 libncurses6 \
      /tmp/mongodb-database-tools.deb; \
    cd /tmp; \
    apt-get download mariadb-client mariadb-client-core mariadb-common; \
    for deb in /tmp/mariadb-*.deb; do dpkg -x "$deb" /opt/mariadb; done; \
    for tool in mariadb mariadb-dump; do \
      ln -sf "/opt/mariadb/usr/bin/${tool}" "/usr/local/bin/${tool}"; \
    done; \
    /usr/local/bin/mariadb-dump --version; \
    /usr/local/bin/mariadb --version; \
    mysqldump --version; \
    rm -rf /tmp/*.deb /var/lib/apt/lists/*

# --- final ------------------------------------------------------------------

FROM engine-${ENGINE} AS final

ARG ENGINE
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

LABEL org.opencontainers.image.title="vaultd" \
      org.opencontainers.image.description="Database backups to S3-compatible storage" \
      org.opencontainers.image.source="https://github.com/curruwilla/vaultd" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}" \
      io.vaultd.variant="${ENGINE}"

# `vaultd doctor` reports this, and refuses a config whose engine is not in the
# image with a message that names the image to use instead.
ENV VAULTD_VARIANT="${ENGINE}" \
    VAULTD_CONFIG=/etc/vaultd/vaultd.yaml

COPY --from=build /out/vaultd /usr/local/bin/vaultd

# The downloader and the keyring tooling were build-time only. Nothing vaultd
# does needs root either, and nothing it writes belongs to the image: the
# bucket is the state, and the config is mounted read-only.
RUN set -eux; \
    apt-get purge -y --auto-remove curl gnupg 2>/dev/null || true; \
    rm -rf /var/lib/apt/lists/*; \
    useradd --system --uid 10001 --home-dir /var/lib/vaultd --create-home vaultd; \
    install -d -o vaultd -g vaultd /etc/vaultd
USER 10001:10001

WORKDIR /var/lib/vaultd
EXPOSE 8080

# The daemon is the default (decision D4); `docker run … backup prod-pg` still
# works, because the entrypoint is the binary and this is only the argument.
#
# There is no HEALTHCHECK on purpose: the daemon's listen address is
# configurable, and a healthcheck that only proves the binary can print its
# version would be worse than none. Point a probe at /healthz.
ENTRYPOINT ["/usr/local/bin/vaultd"]
CMD ["serve"]
