DSN ?= postgres://loom:loom@localhost:5432/loom?sslmode=disable
export DND_DSN := $(DSN)

BIN := bin/loom-dnd

.PHONY: help up down build migrate topic-list user-list campaign-list play clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# ── Infrastructure ─────────────────────────────────────────────────────────────

up: ## Start PostgreSQL 16 via Docker (from the loom root)
	docker compose -f ../../docker-compose.yml up -d
	@echo "Waiting for Postgres..."
	@until docker compose -f ../../docker-compose.yml exec postgres \
		pg_isready -U loom -d loom >/dev/null 2>&1; do sleep 1; done
	@echo "Postgres is ready."

down: ## Stop PostgreSQL
	docker compose -f ../../docker-compose.yml down

# ── Build ───────────────────────────────────────────────────────────────────────

build: ## Build the loom-dnd binary to bin/loom-dnd
	@mkdir -p bin
	go build -o $(BIN) .
	@echo "Built $(BIN)"

# ── Database ───────────────────────────────────────────────────────────────────

migrate: build ## Apply loom + DND schemas to the database
	$(BIN) migrate

# ── Quick-start: create a full session in one command ──────────────────────────
#
# Override any variable on the command line:
#   make quickstart TOPIC_NAME="Eberron" SETTING="steampunk" USERNAME="alice" \
#                   CHAR_NAME="Aria" CHAR_RACE="Elf" CHAR_CLASS="Rogue"

TOPIC_NAME  ?= Forgotten Realms
SETTING     ?= high-fantasy
EDITION     ?= 5e
USERNAME    ?= adventurer
CAMPAIGN    ?= My First Campaign
CHAR_NAME   ?= Hero
CHAR_RACE   ?= Human
CHAR_CLASS  ?= Fighter
BACKGROUND  ?= Folk Hero

quickstart: build migrate ## One-shot: migrate + topic + user + campaign + character (then run 'make play')
	@echo ""
	@echo "── Creating topic: $(TOPIC_NAME) ──────────────────────"
	$(BIN) topic create \
		--name    "$(TOPIC_NAME)" \
		--setting "$(SETTING)" \
		--edition "$(EDITION)" \
		|| true          # ignore 'already exists'
	@echo ""
	@echo "── Creating user: $(USERNAME) ────────────────────────"
	$(BIN) user create --username "$(USERNAME)" || true
	@echo ""
	@echo "── Creating campaign: $(CAMPAIGN) ────────────────────"
	$(BIN) campaign create \
		--topic "$(shell echo "$(TOPIC_NAME)" | tr '[:upper:]' '[:lower:]' | tr ' ' '-')" \
		--name  "$(CAMPAIGN)" \
		|| true
	@echo ""
	@echo "── Run 'make list' to see IDs, then:"
	@echo "   make character USERNAME=$(USERNAME) CAMPAIGN_ID=<id>"
	@echo "   make play      USERNAME=$(USERNAME) CAMPAIGN_ID=<id>"

# ── Per-entity helpers ─────────────────────────────────────────────────────────

CAMPAIGN_ID ?= (set CAMPAIGN_ID=<uuid>)

character: build ## Create a character: make character USERNAME=... CAMPAIGN_ID=...
	$(BIN) character create \
		--username  "$(USERNAME)" \
		--campaign  "$(CAMPAIGN_ID)" \
		--name      "$(CHAR_NAME)" \
		--race      "$(CHAR_RACE)" \
		--class     "$(CHAR_CLASS)" \
		--background "$(BACKGROUND)" \
		--roll-stats

sheet: build ## Show character sheet: make sheet USERNAME=... CAMPAIGN_ID=...
	$(BIN) character sheet \
		--username "$(USERNAME)" \
		--campaign "$(CAMPAIGN_ID)"

play: build ## Start/resume a session: make play USERNAME=... CAMPAIGN_ID=...
	$(BIN) play \
		--username "$(USERNAME)" \
		--campaign "$(CAMPAIGN_ID)"

# ── List commands ──────────────────────────────────────────────────────────────

topic-list: build ## List all D&D topics/settings
	$(BIN) topic list

user-list: build ## List all players
	$(BIN) user list

campaign-list: build ## List all campaigns
	$(BIN) campaign list

list: topic-list user-list campaign-list ## List topics, users, and campaigns

# ── Maintenance ────────────────────────────────────────────────────────────────

clean: ## Remove the built binary
	rm -rf bin/
