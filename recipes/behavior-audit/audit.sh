#!/usr/bin/env bash
# audit.sh - derive a curated behavior-lessons file from ax session history.
#
# Searches session transcripts for a topic, pulls the top matches into a corpus,
# and runs one headless worker that writes recurring patterns, failure modes,
# curated lessons, and proposed behavior-file edits to a review file.
#
# This is not persistent learning. The worker re-derives the summary from
# transcripts at call time. A human reviews and applies the edits by hand.
#
# Usage:
#   bash audit.sh [QUERY] [OUT_FILE]
#     QUERY     search term (default: "coordinator behavior")
#     OUT_FILE  output path (default: audit-output.md)
#
# Requires: jq
set -euo pipefail

QUERY="${1:-coordinator behavior}"
OUT="${2:-audit-output.md}"

IDS=$(ax search "$QUERY" --json | jq -r '.ids[:8][]')
if [[ -z "$IDS" ]]; then
  echo "no sessions matched \"$QUERY\"" >&2
  exit 0
fi

CORPUS=$(mktemp /tmp/audit-corpus.XXXXXX)
PROMPT=$(mktemp /tmp/audit-prompt.XXXXXX)

while read -r id; do
  echo "--- session: $id ---" >> "$CORPUS"
  ax read "$id" --format text | tail -80 >> "$CORPUS"
done <<< "$IDS"

cat > "$PROMPT" <<PROMPT_EOF
You are a behavior-audit worker. Corpus:
$(cat "$CORPUS")

Write: recurring successful patterns (with session citations), recurring
failure modes (with citations), curated lessons, and proposed behavior file
edits as [ADD to section X] / [REVISE Y to] blocks. Mark single-instance
observations as 'watch' not 'pattern'.
PROMPT_EOF

ID=$(ax claude - --wait --unattended --max-tokens 400000 --timeout 15m --json \
  < "$PROMPT" | jq -r .id)
ax result "$ID" > "$OUT"
echo "wrote $OUT" >&2
