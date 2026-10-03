#!/usr/bin/env bash
# Manages the launchd jobs:
#   tick   records prices and runs the live strategy (scripts/tick.sh)
#   agent  the portfolio-manager agent reviews and picks strategies (scripts/run.sh)
# Usage: scripts/launchd.sh install <tick|agent> [interval_seconds] | uninstall <tick|agent|all> | status
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
PREFIX="com.dave.agentic-trader"
LEGACY_LABEL="$PREFIX" # single job from before strategies
DOMAIN="gui/$(id -u)"

label()  { echo "$PREFIX.$1"; }
plist()  { echo "$HOME/Library/LaunchAgents/$1.plist"; }
script() { case "$1" in tick) echo "scripts/tick.sh" ;; agent) echo "scripts/run.sh" ;; *) return 1 ;; esac; }
is_loaded() { launchctl print "$DOMAIN/$1" >/dev/null 2>&1; }

check_job() { script "$1" >/dev/null || { echo "unknown job '$1' (tick or agent)" >&2; exit 2; }; }

unload() {
  local l="$1"
  if is_loaded "$l"; then launchctl bootout "$DOMAIN/$l"; fi
  rm -f "$(plist "$l")"
}

install() {
  local job="$1" interval="$2" l p
  check_job "$job"
  [[ "$interval" =~ ^[0-9]+$ && "$interval" -ge 60 ]] || { echo "interval must be >= 60 seconds" >&2; exit 1; }
  [[ -x "$REPO/bin/trader" ]] || { echo "missing bin/trader; run 'make build' first" >&2; exit 1; }
  if [[ "$job" == agent ]]; then command -v claude >/dev/null || { echo "claude not found on PATH" >&2; exit 1; }; fi

  unload "$LEGACY_LABEL" # replaced by the tick/agent jobs
  l="$(label "$job")"; p="$(plist "$l")"
  mkdir -p "$REPO/logs" "$(dirname "$p")"
  # Render with the current shell's PATH so claude/go/jq resolve as they do for you.
  sed -e "s|{{LABEL}}|$l|g" \
      -e "s|{{REPO_DIR}}|$REPO|g" \
      -e "s|{{SCRIPT}}|$(script "$job")|g" \
      -e "s|{{JOB}}|$job|g" \
      -e "s|{{PATH}}|$PATH|g" \
      -e "s|{{HOME}}|$HOME|g" \
      -e "s|{{INTERVAL}}|$interval|g" \
      "$REPO/deploy/launchd.plist.tmpl" > "$p"
  plutil -lint "$p" >/dev/null

  # Reinstall cleanly if already loaded (picks up template/interval changes).
  unload_quiet() { is_loaded "$l" && launchctl bootout "$DOMAIN/$l" 2>/dev/null || true; }
  unload_quiet
  launchctl bootstrap "$DOMAIN" "$p"
  echo "started $job: every ${interval}s (logs/launchd-$job.log)"
}

uninstall() {
  local job="$1"
  if [[ "$job" == all ]]; then
    unload "$(label tick)"; unload "$(label agent)"; unload "$LEGACY_LABEL"
    echo "stopped tick and agent"
    return
  fi
  check_job "$job"
  unload "$(label "$job")"
  echo "stopped $job"
}

status() {
  for job in tick agent; do
    local l; l="$(label "$job")"
    if ! is_loaded "$l"; then
      echo "$job: not running"
      continue
    fi
    echo "$job:"
    launchctl print "$DOMAIN/$l" | grep -E '^[[:space:]]*(state|run interval|runs|last exit code) =' | sed 's/^[[:space:]]*/  /'
  done
  if is_loaded "$LEGACY_LABEL"; then echo "note: old single job $LEGACY_LABEL is still loaded; 'make start' replaces it"; fi
  if [[ -s "$REPO/logs/tick.log" ]]; then
    echo "last tick output:"; tail -n 3 "$REPO/logs/tick.log" | sed 's/^/  /'
  fi
  latest="$(ls -t "$REPO"/logs/run-*.log 2>/dev/null | head -n 1 || true)"
  if [[ -n "$latest" ]]; then
    echo "latest agent run: $latest"; tail -n 2 "$latest" | sed 's/^/  /'
  fi
}

case "${1:-}" in
  install)   install "${2:-}" "${3:-}" ;;
  uninstall) uninstall "${2:-all}" ;;
  status)    status ;;
  *) echo "usage: $0 install <tick|agent> <interval_seconds> | uninstall [tick|agent|all] | status" >&2; exit 2 ;;
esac
