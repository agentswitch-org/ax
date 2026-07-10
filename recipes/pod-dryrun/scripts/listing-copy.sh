#!/usr/bin/env bash
# listing-copy.sh - listing copy generator.
#
# Ships as a dry-run mock: it emits deterministic canned copy for the 3 seed
# niches. To go live, replace the body below the MOCK banner with a worker LLM
# call that writes copy from the design image. The contract (flags in, files
# out) stays the same, so nothing else in the recipe changes.
#
# Usage:
#   listing-copy.sh --niche SLUG --out DIR
#
# Writes:
#   $out/listing.json  { niche, title (<=140 chars), tags (array of 13), description }
#
# Exit 0 on success; prints the listing.json path to stdout.
set -euo pipefail

niche="" outdir=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --niche) niche="$2";  shift 2 ;;
    --out)   outdir="$2"; shift 2 ;;
    *) echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

[[ -z "$niche" ]]  && { echo "ERROR: --niche required" >&2; exit 1; }
[[ -z "$outdir" ]] && { echo "ERROR: --out required" >&2; exit 1; }

echo "MOCK: listing-copy.sh writing canned listing copy (no LLM called)" >&2
mkdir -p "$outdir"

case "$niche" in
  hiking-gifts)
    title="Funny Hiking Gift Shirt Adventure Lover Mountains Outdoor Hiker Tee Camping Birthday"
    tags='["hiking gift","hiking shirt","hiker tee","mountain lover","outdoor gift","camping shirt","nature lover","hiking lover","trail runner","adventure gift","hiker gift","funny hiking","outdoor tee"]'
    desc="For the trail-blazer who never quits. This bold graphic tee is made for hikers who live for the summit and the stories that follow. A perfect birthday, holiday, or just-because gift for the outdoor enthusiast in your life."
    ;;
  dog-mom-tees)
    title="Dog Mom Shirt Fur Mama Gift Funny Dog Owner Tee Animal Lover Women Puppy Birthday"
    tags='["dog mom shirt","dog lover gift","fur mama tee","pet owner shirt","dog mama gift","animal lover tee","puppy mom shirt","funny dog shirt","dog gift women","dog owner gift","pet mom shirt","dog tshirt","dog lover shirt"]'
    desc="Because fur-ever family is the best family. This cozy dog mom tee says everything your heart already knows. Gift it to the dog lover who never misses a snuggle session."
    ;;
  programmer-humor)
    title="Funny Programmer Shirt Software Developer Coding Tee Coder Gift Computer Science Bug"
    tags='["programmer shirt","coding gift","developer tee","software engineer","funny coder shirt","computer science gift","programmer humor","dev shirt","bug fix tee","code lover shirt","tech gift","programmer gift","coding shirt"]'
    desc="Compiled without errors. This graphic tee is for the developer who talks in functions and debugs in dreams. A sharp, clever gift for the coder in your life."
    ;;
  *)
    echo "ERROR: no canned copy for niche '$niche'. Add a case block or use a real LLM worker." >&2
    exit 1
    ;;
esac

cat > "$outdir/listing.json" <<EOF
{
  "niche": "$niche",
  "title": "$title",
  "tags": $tags,
  "description": "$desc",
  "generated_by": "listing-copy (mock)",
  "generated_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF

echo "$outdir/listing.json"
