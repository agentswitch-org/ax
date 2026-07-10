#!/usr/bin/env bash
# ax-scheduled-chain.sh
# Parametrized cron -> ax chain -> sentinel gate -> notify wrapper.
# Swap --task to run any scheduled workflow with the same file.
#
# Usage:
#   bash ax-scheduled-chain.sh --task "TASK STRING" [OPTIONS]
#
# Options:
#   --task TASK       (required) task string for the ax session
#   --run-id ID       run id label (default: chain-YYYYMMDD-HHMMSS)
#   --notify CMD      notify command string; receives report as first argument
#   --sentinel WORD   sentinel word that suppresses delivery (default: SILENT)
#   --behavior FILE   behavior file path (default: behaviors/worker.md)
#   --max-cost N      ax max-cost fence in USD (default: 2.00; binds only on
#                     API auth, inert on the default subscription auth)
#   --max-tokens N    ax token fence (default: 400000; the fence that binds on
#                     subscription auth; counts input+output+cache tokens)
#   --timeout D       ax timeout duration (default: 10m)
#
# All options also accept AX_CHAIN_* env vars (useful in crontab).
#
# Sentinel gate: if the agent's final output contains exactly the sentinel word
# on its own line, delivery is suppressed and the wrapper exits 0 (clean).
# Otherwise the notify command (or stdout) receives the full report.
#
# Example cron entry:
#   0 8 * * * AX_CHAIN_NOTIFY=/usr/local/bin/ax-slack \
#             bash /path/to/ax-scheduled-chain.sh \
#             --task "Search the web for AI news, summarize top 5" \
#             >> ~/.local/state/ax/log/briefing.log 2>&1

set -euo pipefail

TASK="${AX_CHAIN_TASK:-}"
RUN_ID="chain-$(date +%Y%m%d-%H%M%S)"
NOTIFY_CMD="${AX_CHAIN_NOTIFY:-}"
SENTINEL="${AX_CHAIN_SENTINEL:-SILENT}"
BEHAVIOR="${AX_CHAIN_BEHAVIOR:-$(cd "$(dirname "$0")" && pwd)/behaviors/worker.md}"
MAX_COST="${AX_CHAIN_MAX_COST:-2.00}"
MAX_TOKENS="${AX_CHAIN_MAX_TOKENS:-400000}"
TIMEOUT="${AX_CHAIN_TIMEOUT:-10m}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --task)     TASK="$2";       shift 2 ;;
    --run-id)   RUN_ID="$2";     shift 2 ;;
    --notify)   NOTIFY_CMD="$2"; shift 2 ;;
    --sentinel) SENTINEL="$2";   shift 2 ;;
    --behavior) BEHAVIOR="$2";   shift 2 ;;
    --max-cost) MAX_COST="$2";   shift 2 ;;
    --max-tokens) MAX_TOKENS="$2"; shift 2 ;;
    --timeout)  TIMEOUT="$2";    shift 2 ;;
    *) echo "ax-scheduled-chain: unknown flag: $1" >&2; exit 1 ;;
  esac
done

if [[ -z "$TASK" ]]; then
  echo "ax-scheduled-chain: --task required" >&2
  exit 1
fi

# This wrapper is a cron entry point: it should start its own fresh run even if
# someone invokes it from inside an ax session. Clearing the session env makes
# that explicit (the launch will not join, or inherit the fences of, a caller).
unset AX_SESSION_ID AX_DEPTH AX_RUN AX_GROUP AX_PARENT AX_MAX_DEPTH AX_LABELS 2>/dev/null || true

echo "[ax-chain] START run=$RUN_ID sentinel=$SENTINEL" >&2
echo "[ax-chain] task=$(echo "$TASK" | head -1)..." >&2

SESSION_JSON=$(ax claude "$TASK" \
  --behavior "$BEHAVIOR" \
  --wait \
  --unattended \
  --timeout "$TIMEOUT" \
  --max-cost "$MAX_COST" \
  --max-tokens "$MAX_TOKENS" \
  --run "$RUN_ID" \
  --json)

SESSION_ID=$(echo "$SESSION_JSON" | jq -r .id)
echo "[ax-chain] session=$SESSION_ID done" >&2

OUTPUT=$(ax result "$SESSION_ID")
echo "[ax-chain] output_lines=$(echo "$OUTPUT" | wc -l | tr -d ' ')" >&2

# Sentinel gate: suppress delivery if agent reported nothing actionable.
if printf '%s\n' "$OUTPUT" | grep -q "^${SENTINEL}$"; then
  echo "[ax-chain] SUPPRESSED: sentinel '$SENTINEL' matched; no delivery for run=$RUN_ID" >&2
  exit 0
fi

echo "[ax-chain] DELIVERING: sentinel not matched; firing notify for run=$RUN_ID" >&2

if [[ -n "$NOTIFY_CMD" ]]; then
  # shellcheck disable=SC2086
  $NOTIFY_CMD "$OUTPUT"
else
  printf '%s\n' "$OUTPUT"
fi

echo "[ax-chain] DONE run=$RUN_ID" >&2
