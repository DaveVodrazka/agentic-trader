# agentic-trader

An LLM agent (`claude -p`) that trades Solana tokens via Jupiter. It quotes, decides, executes and keeps a narrative of its reasoning between runs. All state lives in a local SQLite database (`trader.db`).

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
make init     # create trader.db with 1000 USDC + 0.05 SOL
make run      # run one trading cycle
make pnl      # see how it's doing
```

## Commands

| Command | What it does |
|---|---|
| `make init` | Create `trader.db`. Imports old JSON state files if present, otherwise deposits `USDC=1000 SOL=0.05` (overridable). |
| `make run` | Run one trading cycle now. `PROMPT="..."` overrides the prompt. |
| `make start` | Trade continuously every `INTERVAL` seconds (default 300) via launchd. |
| `make stop` | Stop continuous trading. |
| `make restart` | Rebuild and restart continuous trading (after code changes). |
| `make status` | Is continuous trading running, last exit code, tail of the latest run. |
| `make logs` | Follow the latest run's log. |
| `make pnl` | Profit & loss at live prices (costs, holdings, activity), saved as a snapshot. |
| `make narrative` | The agent's current narrative (its working memory). |
| `make journal` | The agent's per-run summaries. |
| `make trades` | Executed trades with the agent's reasons and fees. |
| `make build` | Build `bin/trader`. |
| `make test` | Run tests. |
| `make clean` | Remove build output (keeps `trader.db` and logs). |

## Layout

```
cmd/trader/         CLI: MCP server, init, run bookkeeping, reports
internal/venue/     Jupiter quotes, token registry
internal/trading/   executor, ledger, fees
internal/store/     SQLite schema and queries
internal/memory/    narrative + journal
internal/mcpserver/ MCP tools the agent uses
scripts/            run.sh (one cycle), launchd.sh (scheduling)
CLAUDE.md           the agent's instructions
```
