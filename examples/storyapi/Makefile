DSN ?= postgres://loom:loom@localhost:5432/loom?sslmode=disable
PORT ?= 8080
export API_DSN := $(DSN)
export PORT    := $(PORT)

BIN := bin/story-api

.PHONY: help up down build run dev curl-demo clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ── Infrastructure ──────────────────────────────────────────────────────────────

up: ## Start PostgreSQL 16 via Docker (from the loom root)
	docker compose -f ../../../docker-compose.yml up -d
	@echo "Waiting for Postgres..."
	@until docker compose -f ../../../docker-compose.yml exec postgres \
		pg_isready -U loom -d loom >/dev/null 2>&1; do sleep 1; done
	@echo "Postgres is ready."

down: ## Stop PostgreSQL
	docker compose -f ../../../docker-compose.yml down

# ── Build & Run ─────────────────────────────────────────────────────────────────

build: ## Build the story-api binary to bin/story-api
	@mkdir -p bin
	go build -o $(BIN) .
	@echo "Built $(BIN)"

run: build ## Build and run the server (foreground)
	$(BIN)

dev: ## Run the server directly with go run (rebuilds on change with 'entr' if available)
	go run .

# ── Smoke test (requires a running server on PORT=8080) ─────────────────────────
#
# Run:  make up && make dev &
#       make curl-demo

BASE_URL ?= http://localhost:$(PORT)

curl-demo: ## Walk through register → login → list topics → create story → play
	@echo ""
	@echo "── Register ────────────────────────────────────────────────────"
	@RESP=$$(curl -sf -X POST $(BASE_URL)/api/auth/register \
		-H 'Content-Type: application/json' \
		-d '{"username":"demo","password":"password123"}') && \
	echo "$$RESP" | python3 -m json.tool && \
	TOKEN=$$(echo "$$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])") && \
	echo "" && \
	echo "── List topics ─────────────────────────────────────────────────" && \
	curl -sf $(BASE_URL)/api/topics | python3 -m json.tool && \
	echo "" && \
	TOPIC_ID=$$(curl -sf $(BASE_URL)/api/topics | python3 -c "import sys,json; print(json.load(sys.stdin)['topics'][0]['id'])") && \
	echo "Using topic: $$TOPIC_ID" && \
	echo "" && \
	echo "── Create story ────────────────────────────────────────────────" && \
	STORY_RESP=$$(curl -sf -X POST $(BASE_URL)/api/stories \
		-H "Authorization: Bearer $$TOKEN" \
		-H 'Content-Type: application/json' \
		-d "{\"topic_id\":\"$$TOPIC_ID\",\"name\":\"My Demo Story\"}") && \
	echo "$$STORY_RESP" | python3 -m json.tool && \
	STORY_ID=$$(echo "$$STORY_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['story']['id'])") && \
	echo "" && \
	echo "── Play turn 1 ─────────────────────────────────────────────────" && \
	curl -sf -X POST "$(BASE_URL)/api/stories/$$STORY_ID/turns" \
		-H "Authorization: Bearer $$TOKEN" \
		-H 'Content-Type: application/json' \
		-d '{"action":"I stand at the entrance and look around."}' | python3 -m json.tool && \
	echo "" && \
	echo "── Play turn 2 (stream) ────────────────────────────────────────" && \
	curl -sf -X POST "$(BASE_URL)/api/stories/$$STORY_ID/turns/stream" \
		-H "Authorization: Bearer $$TOKEN" \
		-H 'Content-Type: application/json' \
		-d '{"action":"I step inside cautiously."}' && \
	echo "" && \
	echo "── Turn history ────────────────────────────────────────────────" && \
	curl -sf "$(BASE_URL)/api/stories/$$STORY_ID/turns" \
		-H "Authorization: Bearer $$TOKEN" | python3 -m json.tool

# ── Maintenance ─────────────────────────────────────────────────────────────────

clean: ## Remove built binary
	rm -rf bin/
