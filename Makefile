.PHONY: build build-all test test-race test-integration-ci test-integration-local test-all \
        run vet lint fmt tidy clean kind-up kind-down release-snapshot help

# Variables
BINARY_NAME := autotunnel
KIND_CLUSTER := autotunnel-test
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

test-integration-ci: build ## Run integration tests (CI - assumes KIND cluster accessible)
	KUBECONFIG=$(KUBECONFIG) K8S_CONTEXT=kind-$(KIND_CLUSTER) go test -v -tags=integration ./tests/integration/...

test-integration-local: ## Run integration tests locally (Docker-based KIND, auto-cleanup)
	@docker compose -f tests/integration/docker-compose.test.yml up --abort-on-container-exit --exit-code-from test; \
	ret=$$?; \
	docker rm -f $$(docker ps -aq --filter "name=autotunnel-test-") 2>/dev/null || true; \
	docker compose -f tests/integration/docker-compose.test.yml down; \
	exit $$ret

test-all: test test-integration-local ## Run all tests (unit + integration via Docker)

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

## KIND Cluster (Docker-based)

kind-up: ## Start KIND cluster in Docker (for debugging)
	docker compose -f tests/integration/docker-compose.test.yml up -d kind
	@echo "Waiting for cluster to be healthy..."
	@until docker compose -f tests/integration/docker-compose.test.yml exec kind kubectl --kubeconfig=/output/kubeconfig get nodes >/dev/null 2>&1; do sleep 2; done
	@docker compose -f tests/integration/docker-compose.test.yml exec kind cat /output/kubeconfig > $(KUBECONFIG)
	@echo "KIND cluster ready!"

kind-down: ## Tear down KIND cluster
	docker compose -f tests/integration/docker-compose.test.yml exec kind kind delete cluster --name autotunnel-test 2>/dev/null || true
	docker compose -f tests/integration/docker-compose.test.yml down -v
	@rm -f $(KUBECONFIG)

## Release

release-snapshot: ## Local release build (no publish)
	goreleaser release --snapshot --clean

## Help

help: ## Show this help
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ { printf "  %-25s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
