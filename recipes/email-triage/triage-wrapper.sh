#!/usr/bin/env bash
# triage-wrapper.sh - email triage wrapper.
#
# Acts on every message in the maildir. Notifies only when something important
# is found. The default exit is silence.
#
# Usage:
#   ./triage-wrapper.sh [MAILDIR]
#
# Production cron entry:
#   0 */4 * * * /path/to/triage-wrapper.sh ~/Maildir >> ~/.local/state/ax/log/email-triage.log 2>&1
#
# Environment overrides:
#   ARCHIVE_CMD      command to archive a message (default: stub)
#   UNSUBSCRIBE_CMD  command to unsubscribe (default: stub)
#   DRAFT_REPLY_CMD  command to draft a reply (default: stub)
#   NOTIFY_CMD       command to deliver notification (default: echo)
#   BEHAVIOR         ax behavior file for the triage worker
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
MAILDIR="${1:-${HOME}/Maildir}"

ARCHIVE_CMD="${ARCHIVE_CMD:-echo '[ARCHIVE]'}"
UNSUBSCRIBE_CMD="${UNSUBSCRIBE_CMD:-echo '[UNSUB]'}"
DRAFT_REPLY_CMD="${DRAFT_REPLY_CMD:-echo '[DRAFT]'}"
NOTIFY_CMD="${NOTIFY_CMD:-echo '[NOTIFY]'}"
BEHAVIOR="${BEHAVIOR:-${SCRIPT_DIR}/behaviors/email-triage.md}"

# Gather messages from Maildir cur/. Maildir message files carry no extension
# (names like 1697040000.M1P2.host:2,S), so match any regular file.
MESSAGES=""
while IFS= read -r f; do
  MESSAGES="${MESSAGES}${f}"$'\n'
done < <(find "$MAILDIR/cur" -type f 2>/dev/null | sort || true)
MESSAGES="${MESSAGES%$'\n'}"

if [ -z "$MESSAGES" ]; then
  echo "[triage] no messages found in $MAILDIR/cur -- exiting silently"
  exit 0
fi

# Build the task: dump all messages with filename delimiters
TASK="Process the following emails and triage each one per your instructions."$'\n\n'
while IFS= read -r msg; do
  [ -z "$msg" ] && continue
  fname="$(basename "$msg")"
  TASK+="=== FILE: ${fname} ==="$'\n'
  TASK+="$(cat "$msg")"$'\n\n'
done <<< "$MESSAGES"

# Run the triage worker
RUN_ID="email-triage-$(date +%Y%m%d-%H%M%S)"
SESSION_ID="$(ax claude "$TASK" \
  --behavior "$BEHAVIOR" \
  --model haiku \
  --wait \
  --unattended \
  --timeout 3m \
  --max-cost 0.25 \
  --max-tokens 200000 \
  --run "$RUN_ID" \
  --json | jq -r .id)"
TRIAGE_OUTPUT="$(ax result "$SESSION_ID")"

# Parse action lines and dispatch.
# Fields use ||| as separator so maildir filenames containing ":" are preserved.
while IFS= read -r line; do
  if [[ "$line" == "ACTION|||ARCHIVE|||"* ]]; then
    msg_file="${line#ACTION|||ARCHIVE|||}"
    $ARCHIVE_CMD "$msg_file"
  elif [[ "$line" == "ACTION|||UNSUBSCRIBE|||"* ]]; then
    msg_file="${line#ACTION|||UNSUBSCRIBE|||}"
    $UNSUBSCRIBE_CMD "$msg_file"
  elif [[ "$line" == "ACTION|||DRAFT_REPLY|||"*"|||"* ]]; then
    rest="${line#ACTION|||DRAFT_REPLY|||}"
    msg_file="${rest%%|||*}"
    reply_text="${rest#*|||}"
    $DRAFT_REPLY_CMD "$msg_file" "$reply_text"
  fi
done <<< "$TRIAGE_OUTPUT"

# Sentinel gate: if everything was noise the worker emits SILENT and we exit quietly.
# Silence is the normal case; only the absence of SILENT fires delivery.
if echo "$TRIAGE_OUTPUT" | grep -q '^SILENT$'; then
  exit 0
fi

# At least one important message: extract the IMPORTANT summary and notify.
SUMMARY="$(echo "$TRIAGE_OUTPUT" | grep '^IMPORTANT:' | head -1)"
$NOTIFY_CMD "$SUMMARY"
