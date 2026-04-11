.PHONY: help build test test-unit test-integration lint proto up down logs clean

# ── Config ────────────────────────────────────────────────────────────────────
SERVICES := gateway ingest search worker
IMAGE_PREFIX := semantic-search

# ── Help ──────────────────────────────────────────────────────────────────────
help: ## Print this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ── Build ─────────────────────────────────────────────────────────────────────
build: ## Build all service Docker images
	@for svc in $(SERVICES); do \
		echo "Building $$svc..."; \
		docker build --build-arg SERVICE=$$svc -t $(IMAGE_PREFIX)/$$svc:latest . ; \
	done

build-local: ## Build all service binaries locally (requires Go)
	@for svc in $(SERVICES); do \
		echo "Building $$svc..."; \
		go build -o bin/$$svc ./cmd/$$svc ; \
	done

# ── Protobuf ──────────────────────────────────────────────────────────────────
proto: ## Regenerate Go code from .proto files
	@which protoc > /dev/null || (echo "install protoc first: brew install protobuf" && exit 1)
	@which protoc-gen-go > /dev/null || go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@which protoc-gen-go-grpc > /dev/null || go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		proto/service.proto

# ── Tests ─────────────────────────────────────────────────────────────────────
test: test-unit ## Run all tests (default: unit only)

test-unit: ## Run unit tests (no external dependencies)
	go test -v -race -count=1 ./internal/...

test-integration: ## Run integration tests (requires Docker)
	go test -v -race -count=1 -tags integration ./test/...

test-cover: ## Run tests with coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# ── Lint ──────────────────────────────────────────────────────────────────────
lint: ## Run golangci-lint
	@which golangci-lint > /dev/null || go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	golangci-lint run ./...

vet: ## Run go vet
	go vet ./...

# ── Local dev stack ───────────────────────────────────────────────────────────
up: ## Start local dev stack (Postgres, Redis, LocalStack, all services)
	docker compose up --build -d
	@echo ""
	@echo "Services:"
	@echo "  Gateway REST API: http://localhost:8080"
	@echo "  Ingest gRPC:      localhost:9091"
	@echo "  Search gRPC:      localhost:9093"
	@echo ""
	@echo "Wait ~10s for services to be healthy, then:"
	@echo "  make ingest-example"

down: ## Stop local dev stack
	docker compose down

logs: ## Tail logs from all services
	docker compose logs -f gateway ingest search worker

logs-worker: ## Tail only worker logs
	docker compose logs -f worker

# ── Quick dev test ────────────────────────────────────────────────────────────
JWT := $(shell go run ./scripts/gen-jwt/main.go 2>/dev/null || echo "SET_JWT_HERE")

ingest-example: ## Ingest a sample document
	curl -s -X POST http://localhost:8080/v1/documents \
	  -H "Authorization: Bearer $(JWT)" \
	  -H "Content-Type: application/json" \
	  -d '{"document_id":"doc-1","text":"The quick brown fox jumps over the lazy dog","metadata":{"source":"example"}}' \
	  | jq .

search-example: ## Run a semantic search
	curl -s "http://localhost:8080/v1/search?q=a+fast+animal+jumping" \
	  -H "Authorization: Bearer $(JWT)" \
	  | jq .

# ── Backfill ──────────────────────────────────────────────────────────────────
backfill-dry: ## Show stuck pending documents without publishing
	go run ./scripts/backfill --older-than=30m --dry-run=true

backfill-run: ## Re-queue all pending documents older than 30 minutes
	go run ./scripts/backfill --older-than=30m --dry-run=false

# ── Kubernetes ────────────────────────────────────────────────────────────────
k8s-apply-dev: ## Apply dev Kubernetes manifests
	kubectl apply -k k8s/overlays/dev

k8s-apply-prod: ## Apply production Kubernetes manifests
	kubectl apply -k k8s/overlays/prod

k8s-status: ## Show pod status
	kubectl get pods -n semantic-search

# ── Cleanup ───────────────────────────────────────────────────────────────────
clean: ## Remove build artifacts
	rm -rf bin/ coverage.out coverage.html
	docker compose down -v
