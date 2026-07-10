#!/usr/bin/env bash
# check-drop.sh - accept check for the print-on-demand dry run.
# Used as --accept scripts/check-drop.sh on the coordinator launch.
# Also callable standalone for manual verification.
#
# Checks per niche in DROPS_DIR:
#   - design.png exists
#   - listing.json: title <= 140 chars, exactly 13 tags
#   - draft.json: state == "draft", published == false
#
# Usage:
#   bash check-drop.sh ./runs/merch-drop-dryrun/drops
#   DROPS_DIR=./runs/merch-drop-dryrun/drops bash check-drop.sh
#
# Exit 0 = PASS, non-zero = FAIL (with reasons on stdout).
set -euo pipefail

drops="${1:-${DROPS_DIR:-}}"
[[ -z "$drops" ]] && {
  echo "Usage: check-drop.sh <drops-dir>  (or set DROPS_DIR)"
  exit 2
}
[[ -d "$drops" ]] || {
  echo "FAIL: drops dir not found: $drops"
  exit 1
}

failures=0
niche_count=0

for niche_dir in "$drops"/*/; do
  [[ -d "$niche_dir" ]] || continue
  niche=$(basename "$niche_dir")
  ((niche_count++))
  niche_ok=1

  if [[ ! -f "$niche_dir/design.png" ]]; then
    echo "FAIL [$niche]: missing design.png"
    ((failures++)); niche_ok=0
  fi

  listing="$niche_dir/listing.json"
  if [[ ! -f "$listing" ]]; then
    echo "FAIL [$niche]: missing listing.json"
    ((failures++)); niche_ok=0
  else
    title=$(jq -r '.title' "$listing")
    title_len=${#title}
    if [[ $title_len -gt 140 ]]; then
      echo "FAIL [$niche]: title too long (${title_len} chars, max 140)"
      ((failures++)); niche_ok=0
    fi
    tag_count=$(jq '.tags | length' "$listing")
    if [[ "$tag_count" -ne 13 ]]; then
      echo "FAIL [$niche]: expected 13 tags, got $tag_count"
      ((failures++)); niche_ok=0
    fi
  fi

  draft="$niche_dir/draft.json"
  if [[ ! -f "$draft" ]]; then
    echo "FAIL [$niche]: missing draft.json"
    ((failures++)); niche_ok=0
  else
    state=$(jq -r '.state' "$draft")
    published=$(jq -r '.published' "$draft")
    if [[ "$state" != "draft" ]]; then
      echo "FAIL [$niche]: expected state=draft, got state=$state"
      ((failures++)); niche_ok=0
    fi
    if [[ "$published" != "false" ]]; then
      echo "FAIL [$niche]: published=$published (must be false before human approval)"
      ((failures++)); niche_ok=0
    fi
  fi

  if [[ $niche_ok -eq 1 ]]; then
    echo "PASS [$niche]: title=${title_len}ch tags=${tag_count} state=${state} published=${published}"
  fi
done

if [[ $niche_count -eq 0 ]]; then
  echo "FAIL: no niche directories found in $drops"
  exit 1
fi

if [[ $failures -gt 0 ]]; then
  echo ""
  echo "RESULT: FAIL ($failures failures across $niche_count niches)"
  exit 1
fi

echo ""
echo "RESULT: PASS ($niche_count niches, all checks green)"
exit 0
