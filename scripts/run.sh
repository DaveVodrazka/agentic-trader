#!/usr/bin/env bash
# Runs one portfolio review by the agent, with its memory loaded.
# Usage: scripts/run.sh [--requested] ["optional prompt"]
#   --requested  the owner asked for this review: the agent reviews in full
#
# Only one review runs at a time: if another is running, this exits 75.
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

requested=false
default_prompt="Run your portfolio review."
if [[ "${1:-}" == "--requested" ]]; then
  requested=true
  default_prompt="Review the live strategy (requested by the owner)."
  shift
fi

export TRADER_RUN_ID="run-$(date -u +%Y%m%dT%H%M%SZ)"
PROMPT="${1:-$default_prompt}"
TIMEOUT="${TRADER_TIMEOUT:-600}"
mkdir -p logs
LOG="logs/${TRADER_RUN_ID}.log"

# One review at a time. The lock holds our PID so a crashed run's lock is
# taken over; the web UI reads it to show a review in progress.
LOCK="logs/agent.lock"
if ! mkdir "$LOCK" 2>/dev/null; then
  pid="$(cat "$LOCK/pid" 2>/dev/null || true)"
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    echo "another agent review is running (pid $pid); skipping" >&2
    exit 75
  fi
  rm -rf "$LOCK"
  mkdir "$LOCK"
fi
echo $$ >"$LOCK/pid"
trap 'rm -rf "$LOCK"' EXIT

# Records the run start and returns the agent's memory (narrative + journal).
memory="$("$BIN" -db "$DB" begin-run -run-id "$TRADER_RUN_ID" -prompt "$PROMPT" -requested="$requested")"

mcp_config=$(jq -n --arg bin "$BIN" --arg db "$DB" \
  '{mcpServers: {trader: {command: $bin, args: ["-db", $db, "mcp"]}}}')

echo "== ${TRADER_RUN_ID} started $(date -u +%FT%TZ)" | tee "$LOG"

claude -p "$PROMPT" \
  --append-system-prompt "$memory" \
  --mcp-config "$mcp_config" --strict-mcp-config \
  --allowedTools "mcp__trader__market_summary,mcp__trader__get_candles,mcp__trader__strategy_status,mcp__trader__list_strategies,mcp__trader__compare_strategies,mcp__trader__backtest,mcp__trader__set_strategy,mcp__trader__get_balances,mcp__trader__update_narrative" \
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
