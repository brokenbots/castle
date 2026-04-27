.PHONY: help bootstrap tidy build build-overseer build-castle build-parapet plugins \
	test validate dev-castle dev-overseer dev-parapet demo docker-build \
	docker-build-overseer docker-build-castle compose-up compose-down \
	compose-logs clean proto proto-lint proto-check proto-check-drift

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

build: build-overseer build-castle build-parapet plugins ## Build all binaries

plugins: ## Build adapter plugin binaries
	mkdir -p bin
	cd overseer && for d in ./cmd/overlord-adapter-*; do \
		if [ -d "$$d" ]; then \
			name=$${d##*/}; \
			go build -o ../bin/$$name $$d; \
		fi; \
	done

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
	cd overseer && go test -race ./...
	cd castle   && go test -race ./...

ci: build-overseer build-castle ## Run full CI suite: unit tests + example validation
	cd shared   && go test ./...
	cd workflow && go test ./...
	cd overseer && go test -race ./...
	cd castle   && go test -race ./...
	@for f in examples/*.hcl; do ./bin/overseer validate "$$f"; done

test-integration: build-overseer build-castle plugins ## Run integration tests against real Castle + Overseer processes
	@go test -tags integration -timeout 5m ./tests/integration/...

validate: build-overseer ## Validate all example workflows
	@for f in examples/*.hcl; do ./bin/overseer validate "$$f"; done

dev-castle: ## Run castle in dev mode
	cd castle && go run ./cmd/castle --addr :8080 --db ./castle.dev.db

dev-overseer: ## Run overseer against local castle with the build_and_test example
	cd overseer && go run ./cmd/overseer run --castle http://localhost:8080 --workflow ../examples/build_and_test.hcl

dev-parapet: ## Run parapet dev server
	cd parapet && npm run dev

demo: ## Run end-to-end demo: castle + parapet + a multi-step looping workflow
	./scripts/demo.sh

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

# --- Protobuf / buf -----------------------------------------------------------
# The proto schema under proto/overlord/v1 is the source of truth for every
# wire-visible type and RPC (Phase 1.1). Generated Go lives in shared/pb and
# generated TS in parapet/src/gen; both are checked in so consumers do not
# need `buf` locally to build.

proto: ## Regenerate Go and TS code from proto/overlord/v1
	buf generate

proto-lint: ## Lint the proto schema
	buf lint

proto-check: ## Check the proto schema for breaking changes against main
	@out=$$(buf breaking --against '.git#branch=main' 2>&1); \
	code=$$?; \
	echo "$$out"; \
	if [ $$code -ne 0 ]; then \
		if echo "$$out" | grep -q 'had no .proto files'; then \
			echo "note: main has no proto schema yet; skipping breaking check." >&2; \
			exit 0; \
		fi; \
		exit $$code; \
	fi

proto-check-drift: ## Fail if generated code is out of date with the .proto files
	buf generate
	@if ! git diff --quiet -- shared/pb parapet/src/gen; then \
		echo "Generated proto output is out of date. Run 'make proto' and commit the changes." >&2; \
		git --no-pager diff --stat -- shared/pb parapet/src/gen >&2; \
		exit 1; \
	fi
