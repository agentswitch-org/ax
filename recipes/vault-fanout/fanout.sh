#!/usr/bin/env bash
# fanout.sh - fan N workers in parallel over a corpus, join on one verify pass.
#
# Reads pre-split chunks from fixture/chunks/chunk-*.txt, launches one worker
# per chunk in parallel, waits for all of them, then runs a single verify worker
# whose --accept check (check-vault.sh) must pass before the run concludes.
#
# Prepare the chunks first. For a demo corpus:
#   bash ../../tests/recipes/vault-fanout/gen-mbox.sh > fixture/synthetic.mbox
#   bash split-mbox.sh fixture/synthetic.mbox fixture/chunks
# For your own data, write a chunker that emits fixture/chunks/chunk-SLUG.txt.
#
# Usage:
#   bash fanout.sh
set -euo pipefail

cd "$(cd "$(dirname "$0")" && pwd)"

VAULT=./vault
RUN_ID="vault-$(date +%s)"
mkdir -p "$VAULT"

PIDS=()
for CHUNK in fixture/chunks/chunk-*.txt; do
  SLUG="${CHUNK##*/chunk-}"
  SLUG="${SLUG%.txt}"
  ax claude "Read $CHUNK. Extract key topics, commitments, and dates. Write notes to $VAULT/notes-${SLUG}.md." \
    --run "$RUN_ID" --label "role=worker" --label "correspondent=$SLUG" \
    --max-workers 5 --max-tokens 500000 --timeout 10m \
    --wait --json > "/tmp/launch-${SLUG}.json" &
  PIDS+=($!)
done

wait "${PIDS[@]}"

ax claude "Check each file in $VAULT/notes-*.md for required sections.
    Report PASS or FAIL per file. Exit 0 only if all pass." \
    --run "$RUN_ID" --label "role=verifier" \
    --wait --accept ./check-vault.sh
