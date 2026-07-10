#!/usr/bin/env bash
# create-draft.sh - print-on-demand draft adapter (upload + create).
#
# Ships as a dry-run mock: it uploads the design and creates an UNPUBLISHED
# product draft on disk, calling no real API. To go live, replace the two
# bodies below the MOCK banner with real curl calls to your POD backend (using
# $PRINTIFY_API_TOKEN and $PRINTIFY_SHOP_ID): the upload POST returns an image
# id, the create POST returns a product id. Keep the draft state=draft and
# published=false; publishing is a separate, human-gated step (publish.sh).
#
# Usage:
#   create-draft.sh --niche SLUG --image PATH --listing PATH --out DIR
#
# Writes:
#   $out/upload-receipt.json  (the mock upload response)
#   $out/draft.json           (state=draft, published=false)
#
# Exit 0 on success; prints the product id to stdout.
set -euo pipefail

niche="" image="" listing="" outdir=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --niche)   niche="$2";   shift 2 ;;
    --image)   image="$2";   shift 2 ;;
    --listing) listing="$2"; shift 2 ;;
    --out)     outdir="$2";  shift 2 ;;
    *) echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

[[ -z "$niche" ]]   && { echo "ERROR: --niche required" >&2; exit 1; }
[[ -z "$image" ]]   && { echo "ERROR: --image required" >&2; exit 1; }
[[ -z "$listing" ]] && { echo "ERROR: --listing required" >&2; exit 1; }
[[ -z "$outdir" ]]  && { echo "ERROR: --out required" >&2; exit 1; }

echo "MOCK: create-draft.sh uploading and drafting on disk (no real API called)" >&2
mkdir -p "$outdir"

# --- upload (real adapter: POST the image, read back an image id) ---
image_id="mock-img-$(date +%s)"
cat > "$outdir/upload-receipt.json" <<EOF
{
  "id": "$image_id",
  "source_path": "$image",
  "uploaded_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "source": "create-draft (mock)",
  "note": "dry-run placeholder"
}
EOF

# --- create (real adapter: POST the product with the image id and listing) ---
product_id="mock-prod-$(date +%s)"
title=$(jq -r '.title' "$listing")
cat > "$outdir/draft.json" <<EOF
{
  "product_id": "$product_id",
  "niche": "$niche",
  "image_id": "$image_id",
  "title": "$title",
  "state": "draft",
  "published": false,
  "created_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "source": "create-draft (mock)",
  "note": "dry-run placeholder; publish requires human approval via ax ask"
}
EOF

echo "$product_id"
