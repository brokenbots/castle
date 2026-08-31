.PHONY: help bootstrap tidy build build-castle build-parapet test ci dev-castle \
	dev-parapet docker-build compose-up compose-down compose-logs clean proto \
	proto-lint proto-check proto-check-drift

help:
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

bootstrap: ## Install backend and frontend dependencies
	go work sync
	cd parapet && npm ci

tidy: ## Tidy all Go modules
	cd castle && go mod tidy

build: build-castle build-parapet ## Build Castle and Parapet

build-castle: ## Build the Castle server
	mkdir -p bin
	cd castle && go build -o ../bin/castle ./cmd/castle

build-parapet: ## Build the Parapet UI
	cd parapet && npm run build

test: ## Run backend and frontend tests
	cd castle && CGO_ENABLED=1 go test -race ./...
	cd parapet && npm run test:run

ci: build test proto-lint proto-check-drift ## Run all repository gates

dev-castle: ## Run Castle on localhost:8080
	cd castle && go run ./cmd/castle --addr :8080 --db ./castle.dev.db

dev-parapet: ## Run the Parapet development server
	cd parapet && npm run dev

docker-build: ## Build the Castle container image
	docker build -f Dockerfile.castle -t castle:dev .

compose-up: ## Build and start Castle
	docker compose -f compose.local.yml up --build

compose-down: ## Stop Castle
	docker compose -f compose.local.yml down

compose-logs: ## Follow Castle logs
	docker compose -f compose.local.yml logs -f

clean: ## Remove build outputs and local state
	rm -rf bin parapet/dist parapet/.vite
	rm -f castle/*.db

proto: ## Regenerate Go and TypeScript bindings
	buf generate

proto-lint: ## Lint protobuf schemas
	buf lint

proto-check: ## Check protobuf compatibility against main
	buf breaking --against '.git#branch=main'

proto-check-drift: ## Fail when generated bindings are stale
	buf generate
	@if ! git diff --quiet -- parapet/src/gen; then \
		echo "Generated proto output is out of date. Run 'make proto' and commit the changes." >&2; \
		git --no-pager diff --stat -- parapet/src/gen >&2; \
		exit 1; \
	fi
