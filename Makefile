# Agentic trader. Run `make` for the list of commands.

TICK_INTERVAL  ?= 300
AGENT_INTERVAL ?= 3600
DAYS     ?= 30
PROMPT   ?= Run your portfolio review.
PORT     ?= 8080
BIN      := bin/trader

.DEFAULT_GOAL := help
.PHONY: help init build test run review serve start stop restart status logs tick-log pnl narrative journal trades backfill tick strategies strategy set-strategy backtest clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  \033[36m%-13s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

init: build ## Create trader.db (imports old JSON files if present; else USDC= SOL= deposits)
	@set -a; [ -f .env ] && . ./.env; set +a; $(BIN) init $(if $(USDC),-usdc $(USDC)) $(if $(SOL),-sol $(SOL))

build: ## Build the trader binary
	go build -o $(BIN) ./cmd/trader

test: ## Run tests
	go test -race ./...

run: build ## Run one agent portfolio review now (PROMPT="..." to override)
	scripts/run.sh "$(PROMPT)"

review: build ## Ask the agent for a full review of the live strategy now
	scripts/run.sh --requested

serve: build ## Dashboard at http://localhost:PORT (8080): PnL snapshots, strategy, narrative, review button
	@set -a; [ -f .env ] && . ./.env; set +a; $(BIN) serve -addr 127.0.0.1:$(PORT)

start: build ## Start ticks every TICK_INTERVAL (300s) and agent reviews every AGENT_INTERVAL (3600s); market events wake the agent early
	@scripts/launchd.sh install tick $(TICK_INTERVAL)
	@scripts/launchd.sh install agent $(AGENT_INTERVAL)

stop: ## Stop ticks and agent reviews
	@scripts/launchd.sh uninstall all

restart: stop start ## Rebuild and restart both jobs

status: ## Show both jobs, the last tick output and the latest agent run
	@scripts/launchd.sh status

logs: ## Follow the latest agent run log
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

backfill: build ## Fetch missing price bars (FULL=1 refetch all, OLDER=1 extend history back)
	@$(BIN) backfill $(if $(FULL),-full) $(if $(OLDER),-older)

tick: build ## Record prices and run the live strategy once
	@set -a; [ -f .env ] && . ./.env; set +a; $(BIN) tick

strategies: build ## List available strategies and their default params
	@$(BIN) strategy list

strategy: build ## Show the live strategy, its state and recent ticks
	@$(BIN) strategy show

set-strategy: build ## Set the live strategy: STRATEGY=name [PARAMS='{...}'] REASON='...' [FORCE=1]
	@$(BIN) strategy set $(STRATEGY) $(if $(PARAMS),-params '$(PARAMS)') -reason '$(REASON)' $(if $(FORCE),-force)

backtest: build ## Backtest STRATEGY=name [PARAMS='{...}'] [DAYS=30] vs hold-SOL and 50/50
	@$(BIN) backtest $(STRATEGY) $(if $(PARAMS),-params '$(PARAMS)') -days $(DAYS)

tick-log: ## Follow the strategy tick log (trades, halts, errors)
	@touch logs/tick.log && tail -f logs/tick.log

clean: ## Remove build output (keeps trader.db and logs)
	rm -rf bin
