#!/usr/bin/env bash
# Records prices and runs the live strategy once. Scheduled every few minutes.
# Market events (big moves, stop-outs, halts, drawdown) wake the agent job.
# Output (trades, wake-ups, errors) is appended to logs/tick.log.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"
if [[ -f .env ]]; then
  set -a; source .env; set +a
fi
mkdir -p logs
{
  rc=0
  "$REPO/bin/trader" -db "$REPO/trader.db" tick -q -wake "com.dave.agentic-trader.agent" 2>&1 || rc=$?
  if [[ $rc -ne 0 ]]; then echo "$(date -u +%FT%TZ) tick failed (exit $rc)"; fi
} >> logs/tick.log
