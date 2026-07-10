#!/usr/bin/env bash
# run-health-check.sh - deterministic pre-script + LLM formatter.
#
# Stage 1: check-services.sh runs deterministically, produces ground-truth output.
# Stage 2: output is injected into the ax task; a headless LLM formats it.
# Stage 3: sentinel gate -- SILENT suppresses delivery.
# Stage 4: deliver formatted report if not silent.
#
# Usage:
#   FIXTURE=outage    bash run-health-check.sh   # triggers OUTAGE path
#   FIXTURE=no_issues bash run-health-check.sh   # triggers silent path (default)
#   NOTIFY_CMD="curl -s ntfy.sh/my-alerts -d" bash run-health-check.sh
#
# Depth note: from cron, a plain shell, or inside a root ax session, no extra
# flags are needed (the default fence allows a root plus one level of workers).
# Only from inside a worker session (depth 1) pass:
#   AX_MAX_DEPTH_FLAG="--max-depth 2" bash run-health-check.sh
set -euo pipefail

RECIPE_DIR="$(cd "$(dirname "$0")" && pwd)"
NOTIFY_CMD="${NOTIFY_CMD:-echo '[NOTIFY]'}"
MAX_DEPTH_FLAG="${AX_MAX_DEPTH_FLAG:-}"

# Stage 1: deterministic check
echo "[formatter] running check script..."
SCRIPT_OUTPUT=$(bash "$RECIPE_DIR/check-services.sh")
echo "[formatter] check script done ($(echo "$SCRIPT_OUTPUT" | wc -l | tr -d ' ') lines)"
export SCRIPT_OUTPUT

# Stage 2: inject and format
TASK="A deterministic health check script ran. Its output is below.

== SCRIPT OUTPUT START ==
${SCRIPT_OUTPUT}
== SCRIPT OUTPUT END ==

Instructions:
- You are a formatter only. Do NOT run any commands. Do NOT fetch additional
  data. The output above is complete and authoritative.
- If the output contains NO_ISSUES: respond with exactly the word SILENT on
  its own line and stop. Do not add any other text.
- If the output contains OUTAGE_DETECTED: write a concise on-call incident
  summary (under 200 words). List affected services, their status, and
  timestamps. Output the summary and stop. Do not run any commands."

echo "[formatter] launching ax formatter..."
# shellcheck disable=SC2086
ID=$(ax claude "$TASK" \
    --behavior "$RECIPE_DIR/behaviors/lm-formatter.md" \
    --wait \
    --unattended \
    --timeout 5m \
    --max-tokens 200000 \
    $MAX_DEPTH_FLAG \
    --json \
    2>/dev/null | head -1 | jq -r .id)

echo "[formatter] session $ID complete"

# Stage 3: sentinel gate
OUTPUT=$(ax result "$ID" --json 2>/dev/null | jq -r .result)

if echo "$OUTPUT" | grep -q '^SILENT$'; then
  echo "[formatter] gate: SILENT -- delivery suppressed"
  exit 0
fi

# Stage 4: deliver
echo "[formatter] gate: report ready -- delivering"
echo "=== INCIDENT REPORT ==="
echo "$OUTPUT"
echo "======================="
$NOTIFY_CMD "$OUTPUT"
