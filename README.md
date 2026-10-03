# agentic-trader

Algorithmic trading of Solana tokens via Jupiter, managed by an LLM agent (`claude -p`). Strategies (trend, momentum, volatility targeting, rebalancing…) trade automatically every few minutes; every hour, or immediately when the market moves unexpectedly, the agent reviews the market and backtests, picks which strategy runs, and records its reasoning. All state lives in a local SQLite database (`trader.db`).

Trades are currently **simulated** (paper trading): real Jupiter quotes and fee estimates, no on-chain transactions.

## Requirements

- macOS (continuous mode uses launchd)
- Go, [Claude Code](https://claude.com/claude-code) (`claude`), `jq`
- A Jupiter API key from [portal.jup.ag](https://portal.jup.ag) in `.env`:
  ```sh
  echo 'JUPITER_API_KEY=your-key' > .env
  ```

## Quick start

```sh
make init       # create trader.db with 1000 USDC + 0.05 SOL
make backfill   # load price history (~2 min first time)
make run        # agent reviews the market and picks a strategy
make start      # ticks every 5 min, agent hourly + on market events
make pnl        # see how it's doing
```

## Commands

| Command | What it does |
|---|---|
| `make init` | Create `trader.db`. Imports old JSON state files if present, otherwise deposits `USDC=1000 SOL=0.05` (overridable). |
| `make run` | Run one agent portfolio review now. `PROMPT="..."` overrides the prompt. |
| `make review` | Ask the agent for a full review of the live strategy now (backtests, compare, may switch). |
| `make serve` | Local dashboard at http://localhost:8080 (`PORT=`): PnL snapshots and chart, live strategy vs benchmarks, narrative, agent reports, and buttons to take a snapshot or request a review. No auth: localhost only. |
| `make start` | Schedule ticks every `TICK_INTERVAL` (300 s) and agent reviews every `AGENT_INTERVAL` (3600 s) via launchd. Ticks wake the agent early on big moves, stop-losses, halts or drawdown. |
| `make stop` | Stop both scheduled jobs. |
| `make restart` | Rebuild and restart both jobs (after code changes). |
| `make status` | Both jobs' state, the last tick output and the latest agent run. |
| `make logs` | Follow the latest agent run's log. |
| `make tick-log` | Follow the strategy tick log (trades, halts, errors). |
| `make pnl` | Profit & loss at live prices (costs, holdings, activity), saved as a snapshot. |
| `make narrative` | The agent's current narrative (its working memory). |
| `make journal` | The agent's per-run summaries. |
| `make trades` | Executed trades with the agent's reasons and fees. |
| `make backfill` | Fetch missing price bars from GeckoTerminal (first run ~2 min, then seconds). `FULL=1` refetches everything, `OLDER=1` extends history another ~41 days back. |
| `make strategies` | List strategies and their default parameters. |
| `make backtest` | Backtest `STRATEGY=name [PARAMS='{...}'] [DAYS=30]` against hold-SOL and 50/50. |
| `make set-strategy` | Make a strategy live: `STRATEGY=name [PARAMS='{...}'] REASON='...' [FORCE=1]`. |
| `make strategy` | Show the live strategy, its state and recent ticks. |
| `make tick` | Record prices and run the live strategy once. |
| `make build` | Build `bin/trader`. |
| `make test` | Run tests. |
| `make clean` | Remove build output (keeps `trader.db` and logs). |

## Layout

```
cmd/trader/         CLI: MCP server, init, run bookkeeping, reports
internal/venue/     Jupiter quotes, token registry
internal/trading/   executor, ledger, fees
internal/strategy/  strategies, risk engine, backtester, live runner
internal/marketdata/ historical price bars (GeckoTerminal)
internal/store/     SQLite schema and queries
internal/memory/    narrative + journal
internal/mcpserver/ MCP tools the agent uses (market data, backtests, set_strategy)
internal/web/       local dashboard (make serve)
scripts/            tick.sh, run.sh (agent review), launchd.sh (scheduling)
CLAUDE.md           the agent's instructions
```
