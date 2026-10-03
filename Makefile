# Agentic trader. Run `make` for the list of commands.

INTERVAL ?= 300
PROMPT   ?= Run your trading cycle.
BIN      := bin/trader-mcp

.DEFAULT_GOAL := help
.PHONY: help build test run start stop restart status logs pnl journal trades clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the MCP server binary
	go build -o $(BIN) ./cmd/trader-mcp

test: ## Run tests
	go test -race ./...

run: build ## Run one trading cycle now (PROMPT="..." to override)
	scripts/run.sh "$(PROMPT)"

start: build ## Start continuous trading every INTERVAL seconds (default 300)
	scripts/launchd.sh install $(INTERVAL)

stop: ## Stop continuous trading
	scripts/launchd.sh uninstall

restart: stop start ## Rebuild and restart continuous trading

status: ## Show whether continuous trading is running and the last run
	@scripts/launchd.sh status

logs: ## Follow the latest run log
	@latest=$$(ls -t logs/run-*.log 2>/dev/null | head -n 1); \
	if [ -z "$$latest" ]; then echo "no runs yet"; else tail -f "$$latest"; fi

pnl: ## Show profit & loss vs the initial wallet at live prices
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/pnl

journal: ## Show the agent's journal, one entry per run
	@[ -s journal.jsonl ] && jq -r '"\(.at)  \(.run_id)\n  \(.summary)\n  trades: \(.trade_ids // [] | join(", "))\n"' journal.jsonl || echo "no journal yet"

trades: ## Show executed trades with the agent's reasons
	@[ -s trades.jsonl ] && jq -r '"\(.at)  \(.in) \(.from) -> \(.out) \(.to) @ \(.price)\n  why: \(.reason // "-")\n"' trades.jsonl || echo "no trades yet"

clean: ## Remove build output (keeps wallet, trades, narrative, logs)
	rm -rf bin
