.PHONY: help bootstrap tidy build build-overseer build-castle build-parapet \
	test validate dev-castle dev-overseer dev-parapet docker-build \
	docker-build-overseer docker-build-castle compose-up compose-down \
	compose-logs clean

# Default target: list available targets.
help:
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

bootstrap: ## Install dependencies for all components
	go work sync
	cd parapet && npm install

tidy: ## Run go mod tidy across all Go modules
	cd shared   && go mod tidy
	cd workflow && go mod tidy
	cd overseer && go mod tidy
	cd castle   && go mod tidy

build: build-overseer build-castle build-parapet ## Build all binaries

build-overseer: ## Build overseer binary
	mkdir -p bin
	cd overseer && go build -o ../bin/overseer ./cmd/overseer

build-castle: ## Build castle binary
	mkdir -p bin
	cd castle && go build -o ../bin/castle ./cmd/castle

build-parapet: ## Build parapet UI
	cd parapet && npm run build

test: ## Run all Go tests
	cd shared   && go test ./...
	cd workflow && go test ./...
	cd overseer && go test ./...
	cd castle   && go test ./...

validate: build-overseer ## Validate all example workflows
	@for f in examples/*.hcl; do ./bin/overseer validate "$$f"; done

dev-castle: ## Run castle in dev mode
	cd castle && go run ./cmd/castle --addr :8080 --db ./castle.dev.db

dev-overseer: ## Run overseer against local castle with the build_and_test example
	cd overseer && go run ./cmd/overseer run --castle http://localhost:8080 --workflow ../examples/build_and_test.hcl

dev-parapet: ## Run parapet dev server
	cd parapet && npm run dev

docker-build: docker-build-castle docker-build-overseer ## Build local Docker images for castle and overseer

docker-build-overseer: ## Build overseer Docker image
	docker build -f Dockerfile.overseer -t overlord/overseer:dev .

docker-build-castle: ## Build castle Docker image
	docker build -f Dockerfile.castle -t overlord/castle:dev .

compose-up: ## Build and start local castle+overseer compose stack
	docker compose -f compose.local.yml up --build

compose-down: ## Stop local compose stack and remove containers
	docker compose -f compose.local.yml down

compose-logs: ## Tail logs from local compose stack
	docker compose -f compose.local.yml logs -f

clean: ## Remove build outputs
	rm -rf bin
	rm -f castle/*.db
	rm -rf parapet/dist parapet/.vite
