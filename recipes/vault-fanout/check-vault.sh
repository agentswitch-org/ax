#!/usr/bin/env bash
# check-vault.sh - accept check for the vault fan-out.
#
# Passes (exit 0) only if every vault/notes-*.md exists and carries each
# required section. ax runs this after the verify worker concludes; a non-zero
# exit blocks the run from being marked done. Edit REQUIRED to match the fields
# your extract workers are told to write.
#
# Usage:
#   bash check-vault.sh [VAULT_DIR]   (default: ./vault)
set -euo pipefail

VAULT="${1:-./vault}"
REQUIRED=("topic" "commitment" "date")

files=("$VAULT"/notes-*.md)
if [[ ! -e "${files[0]}" ]]; then
  echo "FAIL: no notes files in $VAULT"
  exit 1
fi

failures=0
for f in "${files[@]}"; do
  missing=()
  for kw in "${REQUIRED[@]}"; do
    grep -qi "$kw" "$f" || missing+=("$kw")
  done
  if [[ ${#missing[@]} -eq 0 ]]; then
    echo "PASS: $(basename "$f")"
  else
    echo "FAIL: $(basename "$f") missing: ${missing[*]}"
    failures=$((failures + 1))
  fi
done

if [[ $failures -gt 0 ]]; then
  echo "RESULT: FAIL ($failures file(s) incomplete)"
  exit 1
fi
echo "RESULT: PASS (${#files[@]} file(s), all sections present)"
exit 0
