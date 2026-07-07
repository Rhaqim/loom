DSN ?= postgres://loom:loom@localhost:5432/loom?sslmode=disable
export LOOM_DSN := $(DSN)

.PHONY: help up down migrate seed example test-engine test-cli test-all build clean generate check-facade check

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

up: ## Start PostgreSQL 16 in Docker
	docker compose up -d
	@echo "Waiting for Postgres to be healthy..."
	@until docker compose exec postgres pg_isready -U loom -d loom >/dev/null 2>&1; do sleep 1; done
	@echo "Postgres is ready."

down: ## Stop and remove containers
	docker compose down

migrate: ## Apply Loom schema to the database
	go run ./cmd/loom-cli migrate

seed: ## Seed example agents and prompts
	go run ./cmd/loom-cli seed examples/story/seed.yaml

example: ## Run the example story application
	go run ./examples/story

test-engine: ## Run engine + harness integration tests (requires Postgres)
	go test -v -count=1 -timeout 120s ./internal/enginetest/...

test-cli: ## Run CLI integration tests (requires Postgres)
	go test -v -count=1 -timeout 60s ./internal/clitest/...

test-all: ## Run all tests (requires Postgres via docker compose up)
	go test -v -count=1 -timeout 180s \
		./internal/enginetest/... \
		./internal/clitest/...

generate: ## Regenerate the root facade (aliases.go) from internal/engine
	go generate ./...

check-facade: ## Fail if aliases.go is stale (run `make generate` and commit)
	go generate ./...
	@if ! git diff --quiet -- aliases.go; then \
		echo "ERROR: aliases.go is out of date. Run 'make generate' and commit the result."; \
		git --no-pager diff -- aliases.go; \
		exit 1; \
	fi
	@echo "facade is up to date."

check: check-facade ## Run non-DB checks: facade sync, vet, build, race tests
	go vet ./...
	go build ./...
	go test -race ./...

build: ## Build the loom-cli binary
	go build -o bin/loom-cli ./cmd/loom-cli

clean: ## Remove build artefacts
	rm -rf bin/
