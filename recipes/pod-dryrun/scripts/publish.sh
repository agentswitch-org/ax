#!/usr/bin/env bash
# publish.sh - print-on-demand publish adapter.
#
# This is the human-gated step. It refuses to run and exits 1 BLOCKED. Publish
# is only reached after the coordinator calls `ax ask` and a human approves.
# Even after wiring a real POD backend, keep this gate: the coordinator must
# receive an explicit human reply before it launches a worker that calls this.
#
# Usage:
#   publish.sh --product-id ID
#
# Exit: always 1 (blocked). A real adapter still runs only downstream of ax ask.
set -euo pipefail

product_id=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --product-id) product_id="$2"; shift 2 ;;
    *) echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

echo "MOCK: publish.sh refusing to publish (human approval required)" >&2
{
  echo ""
  echo "BLOCKED: publish requires human approval."
  echo ""
  echo "The coordinator must call:"
  echo "  ax ask \"N products drafted. Approve to publish?\""
  echo "and receive a human reply before a worker calls publish.sh${product_id:+ --product-id $product_id}."
  echo "In unattended mode, ax ask returns immediately without a reply;"
  echo "the coordinator stops at drafts."
  echo ""
} >&2
exit 1
