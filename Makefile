# WS-00 owns this file.

GO      ?= go
PKGS    := ./...
BIN     := bin/latem
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# The golangci-lint CI pins, read from the file the workflows also read. One
# copy, because two drifted: this target used to run whatever was on PATH while
# CI pinned a version, and a golangci-lint carries the gofmt of the Go release
# it was built with — so a tree could be correctly formatted here and rejected
# there, which is exactly what happened.
GOLANGCI_VERSION := $(shell cat .golangci-lint-version 2>/dev/null)
GOLANGCI        := bin/golangci-lint

# The toolchain CI resolves from go.mod, pinned for lint only.
#
# The linter brings its own formatter, but it asks the local `go` about
# everything else, and v2.12.2 cannot typecheck a newer standard library — it
# panics. Pinning the version without pinning this leaves the correct linter
# unable to run at all on a machine with a newer Go, which is a worse answer
# than the drift it fixes.
GOLANGCI_TOOLCHAIN := $(shell awk '/^go /{print "go"$$2; exit}' go.mod)

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
#
# Across every package, not ./test/... — the build tag is the selector, and a
# directory is not. Three packages kept integration tests outside ./test/ that
# nothing ever compiled: internal/executor/docker (13), internal/cache (3) and
# internal/agent (1). They were correct and they had never run.
#
# Each one skips itself when no daemon answers, so this is safe on a laptop
# without Docker. That is also why CI checks for a daemon before running them:
# a skip and a pass look identical in the summary line.
.PHONY: test-integration
test-integration:
	$(GO) test -race -count=1 -tags=integration $(PKGS)

# Lint is advisory when golangci-lint is absent and strict when it is present
# but the wrong version.
#
# Absent stays advisory on purpose: a contributor without the tool should get a
# meaningful `make check` rather than a tooling error, and CI enforces it
# either way.
#
# A version mismatch is the other thing entirely, and is why this grew teeth. A
# wrong version does not skip — it answers, confidently, about a tree CI will
# judge by different rules. That has already happened here: a commit formatted
# by a newer gofmt passed `make check` and failed the branch. Silence would
# have been better than a green that did not transfer.
#
# bin/ is preferred over PATH because a package manager's copy shadows anything
# installed later, so "install the pinned version" does not reliably change
# which binary runs. The one this project installs is the one this project uses.
.PHONY: lint
lint:
	@want="$(GOLANGCI_VERSION)"; \
	if [ -z "$$want" ]; then \
		echo "no .golangci-lint-version in this tree; cannot tell which linter CI uses"; \
		exit 1; \
	fi; \
	bin="$(GOLANGCI)"; \
	if [ ! -x "$$bin" ]; then \
		if command -v golangci-lint >/dev/null 2>&1; then bin=golangci-lint; else bin=""; fi; \
	fi; \
	if [ -z "$$bin" ]; then \
		echo "golangci-lint not installed — skipping (CI enforces it, at $$want)"; \
		echo "  make lint-install    # fetches exactly that build into $(GOLANGCI)"; \
		exit 0; \
	fi; \
	have="v$$($$bin version --short 2>/dev/null | tr -d ' v')"; \
	if [ "$$have" != "$$want" ]; then \
		echo "golangci-lint $$have is installed; CI pins $$want."; \
		echo "Different builds disagree about formatting, so a green run here would"; \
		echo "not mean a green run in CI. Refusing to give you that answer."; \
		echo "  make lint-install    # fetches $$want into $(GOLANGCI)"; \
		exit 1; \
	fi; \
	GOTOOLCHAIN=$(GOLANGCI_TOOLCHAIN) $$bin run

## lint-install: fetch the exact golangci-lint CI uses into bin/
#
# The released binary, not `go install`: building it from source with whatever
# Go is on this machine embeds that Go's gofmt, which is the difference this
# pin exists to remove.
.PHONY: lint-install
lint-install:
	@set -e; \
	v="$(GOLANGCI_VERSION)"; v_no_v="$${v#v}"; \
	os=$$(uname -s | tr 'A-Z' 'a-z'); \
	case "$$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; \
		*) echo "unsupported architecture $$(uname -m)"; exit 1 ;; esac; \
	name="golangci-lint-$$v_no_v-$$os-$$arch"; \
	url="https://github.com/golangci/golangci-lint/releases/download/$$v/$$name.tar.gz"; \
	echo "fetching $$v for $$os/$$arch"; \
	tmp=$$(mktemp -d); \
	curl -fsSL -o "$$tmp/gcl.tar.gz" "$$url"; \
	tar -xzf "$$tmp/gcl.tar.gz" -C "$$tmp" "$$name/golangci-lint"; \
	mkdir -p $(dir $(GOLANGCI)); \
	mv "$$tmp/$$name/golangci-lint" $(GOLANGCI); \
	rm -rf "$$tmp"; \
	$(GOLANGCI) version

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
