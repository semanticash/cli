# Load local overrides when present.
ifneq (,$(wildcard ./.env))
	include .env
	export
endif

GO ?= go
SQLC ?= sqlc
GOLANGCI_LINT ?= golangci-lint
GOLANGCI_LINT_VERSION ?= v2.11.3
GOLANGCI_LINT_PKG ?= github.com/golangci/golangci-lint/v2/cmd/golangci-lint
INSTALL ?= install

BIN_DIR ?= ./bin
BINARY ?= $(BIN_DIR)/semantica
TOOL_BIN_DIR ?= $(BIN_DIR)/tools
GOLANGCI_LINT_BIN ?= $(TOOL_BIN_DIR)/golangci-lint
COMPLETIONS_DIR ?= ./completions
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS  = -X github.com/semanticash/cli/internal/version.Version=$(VERSION) -X github.com/semanticash/cli/internal/version.Commit=$(COMMIT)

.PHONY: dep generate build completions install test test-race test-e2e lint lint-install fmt check-generated clean enable

dep:
	$(GO) mod download

# Regenerate committed SQL code after query or schema changes.
generate:
	$(SQLC) generate

# Build from committed generated code.
build: dep
	mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/semantica

completions:
	rm -rf $(COMPLETIONS_DIR)
	mkdir -p $(COMPLETIONS_DIR)
	$(GO) run ./cmd/semantica completion bash > $(COMPLETIONS_DIR)/semantica.bash
	$(GO) run ./cmd/semantica completion zsh > $(COMPLETIONS_DIR)/semantica.zsh
	$(GO) run ./cmd/semantica completion fish > $(COMPLETIONS_DIR)/semantica.fish

install: build
	@mkdir -p "$(BINDIR)"
	$(INSTALL) -m 0755 $(BINARY) $(BINDIR)/semantica
	@echo "Installed semantica to $(BINDIR)/semantica"

test:
	$(GO) test ./...

test-race:
	$(GO) test ./... -race -count=1

test-e2e: build
	$(GO) test -tags e2e -count=1 -timeout 60s -v ./e2e/...

lint: $(GOLANGCI_LINT_BIN)
	$(GOLANGCI_LINT_BIN) run

lint-install: $(GOLANGCI_LINT_BIN)

$(GOLANGCI_LINT_BIN):
	mkdir -p $(TOOL_BIN_DIR)
	GOBIN=$(abspath $(TOOL_BIN_DIR)) $(GO) install $(GOLANGCI_LINT_PKG)@$(GOLANGCI_LINT_VERSION)

fmt:
	gofmt -s -w .

check-generated:
	$(SQLC) generate
	git diff --exit-code

clean:
	rm -rf dist/ $(BIN_DIR)/ $(COMPLETIONS_DIR)/

enable: build
	$(BINARY) enable
