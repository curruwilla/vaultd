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

IMAGE   ?= ghcr.io/curruwilla/vaultd
ENGINE  ?= all
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: all build clean test test-race test-integration test-e2e cover fuzz lint lint-fix fmt vet audit validate-example dev-clients install deps-update \
        docker docker-all docker-smoke snapshot release-check help

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
	@# Both the dump clients and the interactive ones: restore feeds a dump
	@# back through mysql or mariadb.
	@ln -sf $(CLIENTS_DIR)/mysql/usr/bin/mysqldump $(CLIENTS_DIR)/bin/mysqldump
	@ln -sf $(CLIENTS_DIR)/mysql/usr/bin/mysql $(CLIENTS_DIR)/bin/mysql
	@ln -sf $(CLIENTS_DIR)/mariadb/usr/bin/mariadb-dump $(CLIENTS_DIR)/bin/mariadb-dump
	@ln -sf $(CLIENTS_DIR)/mariadb/usr/bin/mariadb $(CLIENTS_DIR)/bin/mariadb
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

## docker: Build the container image for this host (ENGINE=all|pg17|mysql8|…)
docker:
	docker build \
		--build-arg ENGINE=$(ENGINE) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE):$(VERSION)$(if $(filter-out all,$(ENGINE)),-$(ENGINE)) .

## docker-all: Build every image variant for every release platform (needs buildx)
docker-all:
	@for engine in all pg14 pg15 pg16 pg17 pg18 mysql8 mariadb11 mongo7; do \
		suffix=$$([ "$$engine" = all ] && echo "" || echo "-$$engine"); \
		echo "==> $(IMAGE):$(VERSION)$$suffix"; \
		docker buildx build --platform $(PLATFORMS) \
			--build-arg ENGINE=$$engine \
			--build-arg VERSION=$(VERSION) \
			--build-arg COMMIT=$(COMMIT) \
			--build-arg DATE=$(DATE) \
			-t $(IMAGE):$(VERSION)$$suffix . || exit 1; \
	done

## docker-smoke: Check that a built image reports its variant and finds its clients
docker-smoke: docker
	docker run --rm $(IMAGE):$(VERSION)$(if $(filter-out all,$(ENGINE)),-$(ENGINE)) version
	docker run --rm -v $(CURDIR)/examples:/cfg:ro \
		--env-file examples/example.env \
		$(IMAGE):$(VERSION)$(if $(filter-out all,$(ENGINE)),-$(ENGINE)) validate -c /cfg/config.yaml

## snapshot: Build the release artifacts locally, without publishing (needs goreleaser)
snapshot:
	goreleaser release --snapshot --clean --skip=sign,publish

## release-check: Everything the release workflow runs, before tagging
release-check: fmt lint test validate-example
	$(GO) test -race ./...
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...
	goreleaser check

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
