#!/usr/bin/env bash
# check.sh - sample accept check for the cost-routing recipe.
#
# Replace this with a check specific to your task. ax runs the check command
# as-is (no arguments) in the launch directory: once when the session tags
# --outcome success, and once by the --wait process after a headless run exits.
# Exit 0 = accepted; non-zero = rejected.
#
# This example: output.json must exist and pass the schema check.

OUTPUT="${OUTPUT_FILE:-./output.json}"

if [[ ! -f "$OUTPUT" ]]; then
  echo "FAIL: $OUTPUT not found" >&2
  exit 1
fi

if ! python3 -c "
import json, sys
d = json.load(open('$OUTPUT'))
assert 'status' in d and d['status'] == 'ok', 'missing or wrong status'
assert 'result' in d and len(d['result']) > 0, 'empty result'
print('OK:', json.dumps(d))
" 2>&1; then
  exit 1
fi

exit 0
