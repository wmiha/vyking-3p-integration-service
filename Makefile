# Run from WSL/Linux/macOS (GNU make + bash). On Windows: `wsl make <target>`.
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

APP     := vyking-3p-integration-service
BIN_DIR := bin
GO      ?= go
APP_ENV ?= local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

export APP_ENV

##@ Help
.PHONY: help
help: ## Show available targets
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} \
		/^##@/ { printf "\n%s\n", substr($$0, 5) } \
		/^[a-zA-Z0-9_-]+:.*##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

##@ Development
.PHONY: tidy fmt vet lint test cover build run clean
tidy: ## Tidy go.mod/go.sum
	$(GO) mod tidy

fmt: ## Format all Go code
	gofmt -s -w .

vet: ## Run go vet
	$(GO) vet ./...

lint: vet ## Check formatting and run go vet
	@out="$$(gofmt -s -l .)"; if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

test: ## Run unit tests with the race detector
	$(GO) test -race -count=1 ./...

cover: ## Run unit tests with coverage report (coverage.out)
	$(GO) test -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

build: ## Build the server binary into bin/
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/server ./cmd/server

run: ## Run the service on the host (APP_ENV=local by default)
	$(GO) run ./cmd/server

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) coverage.out
