# Makefile for llmeval

# ---- Configuration -------------------------------------------------------
BINARY      := server
PKG         := ./cmd/server
BIN_DIR     := bin
BIN         := $(BIN_DIR)/$(BINARY)

IMAGE       ?= llmeval
TAG         ?= dev

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w

GO          ?= go

# ---- Meta ----------------------------------------------------------------
.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help.
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# ---- Development ---------------------------------------------------------
.PHONY: run
run: ## Run the server locally.
	$(GO) run $(PKG)

.PHONY: build
build: ## Build the server binary into ./bin.
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) $(PKG)

.PHONY: install
install: ## Install the server binary into GOBIN.
	$(GO) install -trimpath -ldflags="$(LDFLAGS)" $(PKG)

# ---- Dependencies -------------------------------------------------------
.PHONY: deps
deps: ## Download module dependencies.
	$(GO) mod download

.PHONY: tidy
tidy: ## Tidy and verify go.mod/go.sum.
	$(GO) mod tidy
	$(GO) mod verify

# ---- Quality ------------------------------------------------------------
.PHONY: fmt
fmt: ## Format all Go source.
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

.PHONY: test
test: ## Run tests.
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run tests with the race detector.
	$(GO) test -race ./...

.PHONY: cover
cover: ## Run tests and write a coverage profile.
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

.PHONY: check
check: fmt vet test ## Run fmt, vet and tests.

# ---- Docker -------------------------------------------------------------
.PHONY: docker-build
docker-build: ## Build the Docker image ($(IMAGE):$(TAG)).
	docker build -t $(IMAGE):$(TAG) .

.PHONY: docker-run
docker-run: ## Run the Docker image, publishing port 9090.
	docker run --rm -p 9090:9090 --env-file .env $(IMAGE):$(TAG)

# ---- Housekeeping -------------------------------------------------------
.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf $(BIN_DIR) coverage.out
