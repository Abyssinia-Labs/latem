# WS-00 owns this file.

GO      ?= go
PKGS    := ./...
BIN     := bin/latem
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.DEFAULT_GOAL := check

## check: build, vet, test, lint — the gate every workstream must pass
.PHONY: check
check: build vet test lint
	@echo "ok"

.PHONY: build
build:
	$(GO) build $(PKGS)

.PHONY: vet
vet:
	$(GO) vet $(PKGS)

.PHONY: test
test:
	$(GO) test -race -count=1 $(PKGS)

# Integration tests need Docker and are build-tagged, so they stay out of the
# default gate. CI runs them on a job that has a daemon.
.PHONY: test-integration
test-integration:
	$(GO) test -race -count=1 -tags=integration ./test/...

# Lint is advisory locally and enforced in CI. A contributor without
# golangci-lint installed should still be able to run `make check` and get a
# meaningful answer rather than a tooling error.
.PHONY: lint
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed — skipping (CI enforces it)"; \
		echo "  brew install golangci-lint"; \
	fi

.PHONY: fmt
fmt:
	$(GO) fmt $(PKGS)
	$(GO) run golang.org/x/tools/cmd/goimports@latest -w . 2>/dev/null || true

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: latem
latem:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/latem

.PHONY: clean
clean:
	rm -rf bin/

.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
