#!/usr/bin/env bash
# Runs one trading cycle of the agent with its memory loaded.
# Usage: scripts/run.sh ["optional prompt"]
#
# Env:
#   TRADER_TIMEOUT   max seconds per cycle before it is killed (default 600)
#   JUPITER_API_KEY  optional; may also be set in .env at the repo root
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"

# Optional local secrets/config (not committed).
if [[ -f .env ]]; then
  set -a; source .env; set +a
fi

BIN="$REPO/bin/trader"
DB="$REPO/trader.db"
if [[ ! -x "$BIN" ]]; then
  echo "missing $BIN; run 'make build' first" >&2
  exit 1
fi

export TRADER_RUN_ID="run-$(date -u +%Y%m%dT%H%M%SZ)"
PROMPT="${1:-Run your trading cycle.}"
TIMEOUT="${TRADER_TIMEOUT:-600}"
mkdir -p logs
LOG="logs/${TRADER_RUN_ID}.log"

# Records the run start and returns the agent's memory (narrative + journal).
memory="$("$BIN" -db "$DB" begin-run -run-id "$TRADER_RUN_ID" -prompt "$PROMPT")"

mcp_config=$(jq -n --arg bin "$BIN" --arg db "$DB" \
  '{mcpServers: {trader: {command: $bin, args: ["-db", $db, "mcp"]}}}')

echo "== ${TRADER_RUN_ID} started $(date -u +%FT%TZ)" | tee "$LOG"

claude -p "$PROMPT" \
  --append-system-prompt "$memory" \
  --mcp-config "$mcp_config" --strict-mcp-config \
  --allowedTools "mcp__trader__get_quote,mcp__trader__execute,mcp__trader__get_balances,mcp__trader__update_narrative" \
  > >(tee -a "$LOG") 2>&1 &
pid=$!

# Watchdog: a hung cycle would otherwise block every future scheduled run.
( sleep "$TIMEOUT" && kill "$pid" 2>/dev/null && echo "== killed after ${TIMEOUT}s" >>"$LOG" ) &
watchdog=$!

rc=0
wait "$pid" || rc=$?
kill "$watchdog" 2>/dev/null || true
wait "$watchdog" 2>/dev/null || true

echo "== ${TRADER_RUN_ID} finished $(date -u +%FT%TZ) exit=${rc}" | tee -a "$LOG"

# Store the outcome and snapshot the portfolio (equity curve).
"$BIN" -db "$DB" end-run -run-id "$TRADER_RUN_ID" -exit-code "$rc" -log "$LOG" | tee -a "$LOG" || true
exit "$rc"
