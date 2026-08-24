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

.PHONY: all build clean test test-race cover fuzz lint lint-fix fmt vet audit validate-example install deps-update help

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
