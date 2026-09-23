.PHONY: help run test fmt clean

help: ## Show available commands
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

run: ## Run the local media-story server using .env values
	@set -a; [ ! -f .env ] || . ./.env; set +a; go run .

test: ## Compile and test the example
	go test .

fmt: ## Format the Go source
	gofmt -w main.go

clean: ## Remove the local SQLite database and its journal files
	rm -f db.sqlite db.sqlite-journal db.sqlite-shm db.sqlite-wal
