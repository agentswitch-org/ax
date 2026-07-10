#!/usr/bin/env bash
# notify-stub.sh - logging notify adapter for testing.
# In production, replace with ax-slack, ax-telegram, ntfy, or any delivery binary.
# Receives the report text as first argument.
LOG="${AX_CHAIN_NOTIFY_LOG:-/tmp/ax-chain-notify.log}"
TIMESTAMP=$(date '+%Y-%m-%d %H:%M:%S')
{
  echo "=== NOTIFY $TIMESTAMP ==="
  printf '%s\n' "$1"
  echo "=== END NOTIFY ==="
  echo ""
} >> "$LOG"
echo "[notify-stub] logged to $LOG" >&2
