# benchctl — SCPI bench-instrument CLI
#
# `go` switches to the toolchain pinned in go.mod (go1.27.4) automatically.

GO        ?= go
BIN       ?= bin
PKG       := ./...
CMD       := ./cmd/benchctl
NAME      := benchctl
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS   := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
GOFLAGS   ?= -trimpath
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

export CGO_ENABLED = 0

.DEFAULT_GOAL := all

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: all
all: fmt-check vet test build ## Check formatting, vet, test and build

.PHONY: build
build: ## Build bin/benchctl (static)
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/$(NAME) $(CMD)

.PHONY: install
install: ## Install benchctl into GOBIN
	$(GO) install $(GOFLAGS) -ldflags '$(LDFLAGS)' $(CMD)

.PHONY: cross
cross: ## Cross-compile for $(PLATFORMS) into bin/
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=$(BIN)/$(NAME)-$$os-$$arch; \
		echo "  $$out"; \
		GOOS=$$os GOARCH=$$arch $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out $(CMD) || exit 1; \
	done

.PHONY: test
test: ## Unit tests (no hardware)
	$(GO) test -count=1 $(PKG)

.PHONY: race
race: ## Unit tests with the race detector
	CGO_ENABLED=1 $(GO) test -count=1 -race $(PKG)

.PHONY: cover
cover: ## Unit tests with coverage report (bin/coverage.html)
	@mkdir -p $(BIN)
	$(GO) test -count=1 -coverpkg=./internal/...,./cmd/... -coverprofile=$(BIN)/coverage.out $(PKG)
	$(GO) tool cover -func=$(BIN)/coverage.out | tail -1
	$(GO) tool cover -html=$(BIN)/coverage.out -o $(BIN)/coverage.html

.PHONY: integration
integration: ## Hardware tests (needs the bench and its inventory; drives FG outputs)
	$(GO) test -count=1 -tags bench -v ./internal/bench/integration/ ./internal/bench/adapter/transport/vxi11/

.PHONY: vet
vet: ## go vet (including bench-tagged files)
	$(GO) vet $(PKG)
	$(GO) vet -tags bench $(PKG)

.PHONY: lint
lint: vet ## go vet + golangci-lint (must be built with Go >= 1.27)
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not installed; ran go vet only"; exit 0; fi; \
	{ golangci-lint run $(PKG) && golangci-lint run --build-tags bench $(PKG); } || { echo; \
		echo "hint: golangci-lint must be built with Go >= 1.27 for this module:"; \
		echo "  GOTOOLCHAIN=go1.27.4 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; exit 1; }

.PHONY: fmt
fmt: ## Format sources
	gofmt -w cmd internal

.PHONY: fmt-check
fmt-check: ## Fail if sources are not gofmt'ed
	@out=$$(gofmt -l cmd internal); if [ -n "$$out" ]; then echo "not formatted:"; echo "$$out"; exit 1; fi

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN)
