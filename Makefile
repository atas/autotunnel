.PHONY: build build-all test test-race test-integration test-all run vet lint fmt tidy clean \
        integration-up integration-down integration-test release-snapshot help

# Variables
BINARY_NAME := lazyfwd
KIND_CLUSTER := lazyfwd-test
KUBECONFIG := $(shell pwd)/.kubeconfig-test

# Default target
.DEFAULT_GOAL := help

## Build

build: ## Build the binary
	go build -o $(BINARY_NAME) .

build-all: ## Build for all platforms (via goreleaser, local only)
	goreleaser build --snapshot --clean

## Test

test: ## Run unit tests
	go test -v ./...

test-race: ## Run unit tests with race detector
	go test -v -race ./...

test-integration: build ## Run integration tests (requires k3d cluster running)
	KUBECONFIG=$(KUBECONFIG) go test -v -tags=integration ./tests/integration/...

test-all: test integration-up test-integration integration-down ## Run all tests (unit + integration)

## Development

run: build ## Build and run with example config
	./$(BINARY_NAME) --config example-config.yaml --verbose

vet: ## Run go vet
	go vet ./...

lint: vet ## Run go vet + golangci-lint
	golangci-lint run ./...

fmt: ## Format code
	go fmt ./...

tidy: ## Run go mod tidy
	go mod tidy

clean: ## Clean build artifacts
	rm -f $(BINARY_NAME)
	rm -rf dist/

## Integration Test Infrastructure (Docker Compose)

integration-up: ## Start KIND cluster and deploy test services (via Docker Compose)
	docker compose -f tests/integration/docker-compose.test.yml up -d kind
	@echo "Waiting for cluster to be healthy..."
	@until docker compose -f tests/integration/docker-compose.test.yml exec kind kubectl --kubeconfig=/output/kubeconfig get nodes >/dev/null 2>&1; do sleep 2; done
	@docker compose -f tests/integration/docker-compose.test.yml exec kind cat /output/kubeconfig > $(KUBECONFIG)
	@echo "Integration environment ready!"

integration-down: ## Tear down KIND cluster
	docker compose -f tests/integration/docker-compose.test.yml exec kind kind delete cluster --name lazyfwd-test 2>/dev/null || true
	docker compose -f tests/integration/docker-compose.test.yml down -v
	@rm -f $(KUBECONFIG)

integration-test: ## Run integration tests (via Docker Compose, all-in-one)
	docker compose -f tests/integration/docker-compose.test.yml run --rm test

## Release

release-snapshot: ## Local release build (no publish)
	goreleaser release --snapshot --clean

## Help

help: ## Show this help
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ { printf "  %-20s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
