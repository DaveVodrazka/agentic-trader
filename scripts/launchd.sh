#!/usr/bin/env bash
# Manages the launchd agent that runs scripts/run.sh on an interval.
# Usage: scripts/launchd.sh install [interval_seconds] | uninstall | status
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
LABEL="com.dave.agentic-trader"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
DOMAIN="gui/$(id -u)"

is_loaded() { launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; }

install() {
  local interval="${1:-300}"
  [[ "$interval" =~ ^[0-9]+$ && "$interval" -ge 60 ]] || { echo "interval must be >= 60 seconds" >&2; exit 1; }
  [[ -x "$REPO/bin/trader-mcp" ]] || { echo "missing bin/trader-mcp; run 'make build' first" >&2; exit 1; }
  command -v claude >/dev/null || { echo "claude not found on PATH" >&2; exit 1; }

  mkdir -p "$REPO/logs" "$(dirname "$PLIST")"
  # Render with the current shell's PATH so claude/go/jq resolve as they do for you.
  sed -e "s|{{LABEL}}|$LABEL|g" \
      -e "s|{{REPO_DIR}}|$REPO|g" \
      -e "s|{{PATH}}|$PATH|g" \
      -e "s|{{HOME}}|$HOME|g" \
      -e "s|{{INTERVAL}}|$interval|g" \
      "$REPO/deploy/launchd.plist.tmpl" > "$PLIST"
  plutil -lint "$PLIST" >/dev/null

  # Reinstall cleanly if already loaded (picks up template/interval changes).
  is_loaded && launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
  launchctl bootstrap "$DOMAIN" "$PLIST"
  echo "started $LABEL: every ${interval}s, logs in $REPO/logs/"
}

uninstall() {
  if is_loaded; then
    launchctl bootout "$DOMAIN/$LABEL"
  fi
  rm -f "$PLIST"
  echo "stopped $LABEL"
}

status() {
  if ! is_loaded; then
    echo "$LABEL: not running"
    return
  fi
  launchctl print "$DOMAIN/$LABEL" | grep -E '^[[:space:]]*(state|run interval|runs|last exit code|pid) =' | sed 's/^[[:space:]]*/  /'
  latest="$(ls -t "$REPO"/logs/run-*.log 2>/dev/null | head -n 1 || true)"
  if [[ -n "$latest" ]]; then
    echo "latest run log: $latest"
    tail -n 3 "$latest" | sed 's/^/  /'
  fi
}

case "${1:-}" in
  install)   install "${2:-}" ;;
  uninstall) uninstall ;;
  status)    status ;;
  *) echo "usage: $0 install [interval_seconds] | uninstall | status" >&2; exit 2 ;;
esac
