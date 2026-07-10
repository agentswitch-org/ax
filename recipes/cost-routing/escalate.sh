#!/usr/bin/env bash
# escalate.sh - cost-tiered model routing wrapper.
#
# Runs a task on the cheapest tier first. If the accept check fails (any
# non-zero --wait exit), escalates to the next tier. Stops on the first pass.
#
# Usage:
#   bash escalate.sh "TASK" ["bash ./check.sh"]
#
# EXIT:
#   0  a tier passed
#   1  every tier failed
set -euo pipefail

TASK="${1:?task required}"
CHECK="${2:-bash ./check.sh}"

for MODEL in haiku sonnet opus; do
  echo "[router] tier: $MODEL" >&2
  ax claude "$TASK" --model "$MODEL" --max-tokens 100000 \
    --wait --accept "$CHECK" && exit 0
  echo "[router] tier $MODEL failed, escalating" >&2
done

echo "[router] all tiers failed" >&2
exit 1
