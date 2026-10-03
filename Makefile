# Agentic trader. Run `make` for the list of commands.

INTERVAL ?= 300
PROMPT   ?= Run your trading cycle.
BIN      := bin/trader

.DEFAULT_GOAL := help
.PHONY: help init build test run start stop restart status logs pnl narrative journal trades clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

init: build ## Create trader.db (imports old JSON files if present; else USDC= SOL= deposits)
	@set -a; [ -f .env ] && . ./.env; set +a; $(BIN) init $(if $(USDC),-usdc $(USDC)) $(if $(SOL),-sol $(SOL))

build: ## Build the trader binary
	go build -o $(BIN) ./cmd/trader

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

pnl: build ## Show profit & loss at live prices and save it as a snapshot
	@set -a; [ -f .env ] && . ./.env; set +a; $(BIN) pnl

narrative: build ## Show the agent's latest narrative
	@$(BIN) narrative

journal: build ## Show the agent's journal, one entry per run
	@$(BIN) journal

trades: build ## Show executed trades with reasons and fees
	@$(BIN) trades

clean: ## Remove build output (keeps trader.db and logs)
	rm -rf bin
