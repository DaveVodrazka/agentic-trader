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

BIN="$REPO/bin/trader-mcp"
if [[ ! -x "$BIN" ]]; then
  echo "missing $BIN; run 'make build' first" >&2
  exit 1
fi

export TRADER_RUN_ID="run-$(date -u +%Y%m%dT%H%M%SZ)"
PROMPT="${1:-Run your trading cycle.}"
TIMEOUT="${TRADER_TIMEOUT:-600}"
mkdir -p logs
LOG="logs/${TRADER_RUN_ID}.log"

if [[ -s NARRATIVE.md ]]; then
  narrative="$(cat NARRATIVE.md)"
else
  narrative="(none yet — this is your first run)"
fi
if [[ -s journal.jsonl ]]; then
  journal="$(tail -n 5 journal.jsonl)"
else
  journal="(empty)"
fi

memory="# Memory from previous runs

Current time: $(date -u +%Y-%m-%dT%H:%M:%SZ)
Run ID: ${TRADER_RUN_ID}

## Your narrative (NARRATIVE.md)
${narrative}

## Recent journal entries (newest last)
${journal}"

# Absolute paths so the server works regardless of the caller's cwd.
mcp_config=$(jq -n --arg bin "$BIN" --arg repo "$REPO" '{mcpServers: {trader: {
  command: $bin,
  args: ["-wallet", "\($repo)/WALLET.json", "-trades", "\($repo)/trades.jsonl",
         "-narrative", "\($repo)/NARRATIVE.md", "-journal", "\($repo)/journal.jsonl"]
}}}')

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
exit "$rc"
