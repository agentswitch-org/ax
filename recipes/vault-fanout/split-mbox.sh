#!/usr/bin/env bash
# split-mbox.sh - split an mbox file into per-sender chunk files.
# Usage: ./split-mbox.sh fixture/synthetic.mbox fixture/chunks/
# Writes fixture/chunks/chunk-SENDER.txt for each distinct From address.
set -euo pipefail

MBOX="${1:?usage: split-mbox.sh <mbox> <out-dir>}"
OUTDIR="${2:?usage: split-mbox.sh <mbox> <out-dir>}"
mkdir -p "$OUTDIR"

current_sender=""
current_buf=""

flush() {
  [[ -z "$current_sender" ]] && return
  local slug
  slug=$(echo "$current_sender" | sed 's/@.*//' | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9')
  local f="$OUTDIR/chunk-${slug}.txt"
  printf '%s\n' "$current_buf" >> "$f"
}

while IFS= read -r line; do
  if [[ "$line" =~ ^From\ ([^ ]+)\ .* ]]; then
    flush
    current_sender="${BASH_REMATCH[1]}"
    current_buf="$line"
  else
    current_buf+=$'\n'"$line"
  fi
done < "$MBOX"
flush

echo "split-mbox: wrote chunks to $OUTDIR"
ls -1 "$OUTDIR"
