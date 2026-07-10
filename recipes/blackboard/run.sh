#!/usr/bin/env bash
# run.sh - cross-agent blackboard: producer + critic, headless pipeline.
#
# Two workers coordinate through a shared JSON file on disk. The producer
# appends one item; the critic reads it and appends a verdict. Neither sees the
# other's context, only what was written to the file.
#
# Usage:
#   bash run.sh          # blackboard at /tmp/blackboard.json
#   BB=/path bash run.sh # override the blackboard path
set -euo pipefail

BB="${BB:-/tmp/blackboard.json}"
echo '{"items":[],"verdicts":[]}' > "$BB"

PROD=$(ax claude \
    "Blackboard: $BB. Read it. Append one item to 'items'. Write it back." \
    --wait --json | jq -r .id)
ax wait "$PROD" --timeout 5m
ax result "$PROD"

CRIT=$(ax claude \
    "Blackboard: $BB. Read it. For each item, append a verdict to 'verdicts'. Write it back." \
    --wait --json | jq -r .id)
ax wait "$CRIT" --timeout 5m
ax result "$CRIT"

cat "$BB" | jq .
