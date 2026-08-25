BINARY_NAME := vaultd
GO          := go
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG         := github.com/curruwilla/vaultd/internal/buildinfo
LDFLAGS     := -ldflags "-s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)"

# golangci-lint 2.12 cannot analyze a Go 1.27 standard library (honnef.co IR
# builder panics). Pin the toolchain it runs under until that is released.
LINT_TOOLCHAIN ?= go1.25.5

PG_CLIENT_MAJOR   ?= 16
MONGO_TOOLS_IMAGE ?= mongo:7
CLIENTS_DIR       := $(CURDIR)/.cache/clients

.PHONY: all build clean test test-race test-integration test-e2e cover fuzz lint lint-fix fmt vet audit validate-example dev-clients install deps-update help

all: fmt lint test build

## build: Build the vaultd binary into bin/
build:
	CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/vaultd

## clean: Remove build and coverage artifacts
clean:
	rm -rf bin/ coverage.out coverage.html

## test: Run the test suite
test:
	$(GO) test ./...

## test-race: Run the test suite under the race detector
test-race:
	$(GO) test -race ./...

## test-integration: Run every test including the container-backed ones (needs Docker)
test-integration:
	$(GO) test -tags integration -timeout 900s ./...

## test-e2e: Run the end-to-end acceptance suite (needs Docker)
test-e2e:
	$(GO) test -tags integration -timeout 900s -v ./test/e2e/

## cover: Run tests with coverage and write coverage.html
cover:
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	$(GO) tool cover -func=coverage.out | tail -1

## fuzz: Fuzz the config parser for 60s
fuzz:
	$(GO) test ./internal/config/ -run FuzzParse -fuzz FuzzParse -fuzztime 60s

## lint: Run golangci-lint
lint:
	GOTOOLCHAIN=$(LINT_TOOLCHAIN) golangci-lint run ./...

## lint-fix: Run golangci-lint with --fix
lint-fix:
	GOTOOLCHAIN=$(LINT_TOOLCHAIN) golangci-lint run --fix ./...

## fmt: Format the code
fmt:
	GOTOOLCHAIN=$(LINT_TOOLCHAIN) golangci-lint fmt ./...

## vet: Run go vet
vet:
	$(GO) vet ./...

## audit: Check dependencies for known vulnerabilities
audit:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

## validate-example: Validate the example config with demo values (M0 gate)
validate-example: build
	set -a; . ./examples/example.env; set +a; ./bin/$(BINARY_NAME) validate -c examples/config.yaml

## dev-clients: Unpack the database clients under .cache/ (no root required)
dev-clients:
	@mkdir -p $(CLIENTS_DIR)/debs $(CLIENTS_DIR)/bin
	@cd $(CLIENTS_DIR)/debs && apt-get download \
		postgresql-client-$(PG_CLIENT_MAJOR) postgresql-client-common libpq5 \
		mysql-client-8.0 mysql-client-core-8.0 mysql-common \
		mariadb-client mariadb-client-core mariadb-common libmariadb3
	@# Separate roots: both forks ship a /usr/bin/mysqldump, and which one is
	@# which has to stay unambiguous.
	@for deb in $(CLIENTS_DIR)/debs/postgresql-* $(CLIENTS_DIR)/debs/libpq5*; do dpkg -x $$deb $(CLIENTS_DIR)/pg; done
	@for deb in $(CLIENTS_DIR)/debs/mysql-*; do dpkg -x $$deb $(CLIENTS_DIR)/mysql; done
	@for deb in $(CLIENTS_DIR)/debs/mariadb-* $(CLIENTS_DIR)/debs/libmariadb3*; do dpkg -x $$deb $(CLIENTS_DIR)/mariadb; done
	@for tool in pg_dump pg_dumpall pg_restore; do \
		ln -sf $(CLIENTS_DIR)/pg/usr/lib/postgresql/$(PG_CLIENT_MAJOR)/bin/$$tool $(CLIENTS_DIR)/bin/$$tool; done
	@ln -sf $(CLIENTS_DIR)/mysql/usr/bin/mysqldump $(CLIENTS_DIR)/bin/mysqldump
	@ln -sf $(CLIENTS_DIR)/mariadb/usr/bin/mariadb-dump $(CLIENTS_DIR)/bin/mariadb-dump
	@# The MongoDB tools are not in the distribution archives; take them from
	@# the same image the tests run against.
	@cid=$$(docker create $(MONGO_TOOLS_IMAGE)); \
		docker cp $$cid:/usr/bin/mongodump $(CLIENTS_DIR)/bin/mongodump >/dev/null; \
		docker cp $$cid:/usr/bin/mongorestore $(CLIENTS_DIR)/bin/mongorestore >/dev/null; \
		docker rm $$cid >/dev/null
	@echo
	@echo "Clients unpacked. Export these, then run the integration suite:"
	@echo
	@echo "  export PATH=$(CLIENTS_DIR)/bin:$$PATH"
	@echo "  export LD_LIBRARY_PATH=$(CLIENTS_DIR)/pg/usr/lib/x86_64-linux-gnu:$(CLIENTS_DIR)/mariadb/usr/lib/x86_64-linux-gnu:$(CLIENTS_DIR)/mysql/usr/lib/x86_64-linux-gnu"
	@echo "  export VAULTD_TEST_PG_IMAGE=postgres:$(PG_CLIENT_MAJOR)-alpine"

## install: Download and tidy dependencies
install:
	$(GO) mod download
	$(GO) mod tidy

## deps-update: Update dependencies to their latest patch releases
deps-update:
	$(GO) get -u=patch ./...
	$(GO) mod tidy

## help: Show this help
help:
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
