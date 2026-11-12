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
.PHONY: tidy fmt vet lint test test-integration cover build run
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

IT_COMPOSE := docker compose -f docker-compose.test.yml
# Packages with //go:build integration tests; they share one database, hence -p 1.
IT_PACKAGES := ./test/integration/... ./internal/adapter/repository/...

test-integration: ## Run integration tests against a throwaway MySQL (always torn down; CI-friendly)
	@rc=0; \
	$(IT_COMPOSE) up -d --wait mysql && $(IT_COMPOSE) run --rm flyway migrate || rc=$$?; \
	if [ $$rc -eq 0 ]; then $(GO) test -tags=integration -count=1 -p 1 -v $(IT_PACKAGES) || rc=$$?; fi; \
	$(IT_COMPOSE) down -v --remove-orphans; \
	if [ $$rc -ne 0 ]; then echo "integration tests FAILED (exit $$rc)"; fi; \
	exit $$rc

cover: ## Run unit tests with coverage report (coverage.out)
	$(GO) test -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

build: ## Build the server binary into bin/
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/server ./cmd/server

run: ## Run the service on the host against `make deps-up` (APP_ENV=local)
	$(GO) run -ldflags "-X main.version=$(VERSION)" ./cmd/server

##@ Docker
.PHONY: docker-build up deps-up down clean logs ps migrate migrate-info
COMPOSE ?= docker compose

docker-build: ## Build the service image (distroless)
	docker build --build-arg VERSION=$(VERSION) -t $(APP):$(VERSION) -t $(APP):local .
	@docker image ls $(APP):local --format 'image size: {{.Size}}'

up: ## Build and start MySQL, migrations and the service; waits until healthy
	VERSION=$(VERSION) $(COMPOSE) up -d --build --wait app

deps-up: ## Start only MySQL and run migrations (for `make run` on the host)
	$(COMPOSE) up -d --wait mysql
	$(COMPOSE) run --rm flyway migrate

down: ## Stop the stack, keep the data volume
	$(COMPOSE) down --remove-orphans

clean: ## Stop the stack, delete the data volume (fresh start) and build artifacts
	$(COMPOSE) down -v --remove-orphans
	rm -rf $(BIN_DIR) coverage.out

logs: ## Follow service logs
	$(COMPOSE) logs -f app

ps: ## Show stack status
	$(COMPOSE) ps -a

migrate: ## Apply Flyway migrations to the compose database
	$(COMPOSE) run --rm flyway migrate

migrate-info: ## Show Flyway migration status
	$(COMPOSE) run --rm flyway info

##@ Test data
.PHONY: seed
seed: ## Load deterministic sample data into the compose database (replaces existing data)
	$(GO) run ./scripts/seed -reset

.PHONY: tokens
tokens: ## Print partner test tokens (valid, expired, forged, ...) as TOKEN_*=... lines
	@$(GO) run ./scripts/tokengen -matrix
