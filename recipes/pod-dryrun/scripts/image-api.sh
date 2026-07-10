#!/usr/bin/env bash
# image-api.sh - image generation adapter.
#
# Ships as a dry-run mock: it writes a placeholder PNG and metadata and calls
# no real API. To go live, replace the body below the MOCK banner with a curl
# call to your image API (using $IMAGE_API_KEY) that saves the response body to
# $out/design.png. The contract (flags in, files out) stays the same, so the
# coordinator behavior, the worker task, and the accept check do not change.
#
# Usage:
#   image-api.sh --niche SLUG --concept "CONCEPT TEXT" --out DIR
#
# Writes:
#   $out/design.png       (PNG magic header placeholder)
#   $out/design.png.meta  (JSON metadata)
#   $out/brief.md         (design concept brief, for audit)
#
# Exit 0 on success; prints the design.png path to stdout.
set -euo pipefail

niche="" concept="" outdir=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --niche)   niche="$2";   shift 2 ;;
    --concept) concept="$2"; shift 2 ;;
    --out)     outdir="$2";  shift 2 ;;
    *) echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

[[ -z "$niche" ]]  && { echo "ERROR: --niche required" >&2; exit 1; }
[[ -z "$outdir" ]] && { echo "ERROR: --out required" >&2; exit 1; }

echo "MOCK: image-api.sh writing placeholder PNG (no real API called)" >&2

mkdir -p "$outdir"

# Placeholder body. Real adapter: curl the image API and save the response here.
printf '\x89PNG\r\n\x1a\n' > "$outdir/design.png"

cat > "$outdir/design.png.meta" <<EOF
{
  "niche": "$niche",
  "concept": $(printf '%s' "$concept" | python3 -c 'import sys,json; print(json.dumps(sys.stdin.read()))'),
  "width_px": 4500,
  "height_px": 5400,
  "dpi": 300,
  "has_alpha": true,
  "format": "PNG",
  "source": "image-api (mock)",
  "generated_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF

cat > "$outdir/brief.md" <<EOF
# Design brief: $niche

**Concept:** $concept

**Print spec:** 4500x5400 px, 300 DPI, transparent background (PNG with alpha).
**Source:** image-api mock (dry-run placeholder; production uses a real image API).
EOF

echo "$outdir/design.png"
