#!/usr/bin/env bash
# gen-maildir.sh - write a temp maildir for verifying the email-triage recipe.
#
# Creates a maildir with two messages: one obvious newsletter (noise, expect an
# ARCHIVE/UNSUBSCRIBE action and no notification) and one obvious real request
# (important, expect the sentinel gate to fire a notification). Test apparatus
# for the real path; not part of the recipe bundle.
#
# Usage:
#   bash gen-maildir.sh [MAILDIR]   (default: a fresh mktemp dir, printed to stdout)
set -euo pipefail

MAILDIR="${1:-$(mktemp -d "${TMPDIR:-/tmp}/triage-maildir.XXXXXX")}"
mkdir -p "$MAILDIR/cur" "$MAILDIR/new" "$MAILDIR/tmp"

cat > "$MAILDIR/cur/1700000000.noise:2,S" <<'EOF'
From: Deals Weekly <no-reply@deals.example.com>
To: me@example.com
Date: Mon, 06 Jan 2026 09:00:00 +0000
Subject: 🔥 50% OFF everything this week only!

Shop now before it's gone. Unsubscribe: https://deals.example.com/unsub
EOF

cat > "$MAILDIR/cur/1700000001.important:2,S" <<'EOF'
From: Dana Reyes <dana@client.example.com>
To: me@example.com
Date: Mon, 06 Jan 2026 10:30:00 +0000
Subject: Contract signature needed by Friday

Hi, legal needs your signature on the renewal by Friday or the current terms
lapse. Can you confirm you'll sign, or tell me who should? This is blocking.
EOF

echo "$MAILDIR"
