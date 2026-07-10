#!/usr/bin/env bash
# taste-loop.sh - Taste-gated iteration loop.
#
# A producer emits a deliverable. A fresh reviewer scores it against a rubric and
# returns strict JSON. If the score clears the threshold (or the reviewer passes
# it) the loop stops and emits the deliverable. Otherwise the reviewer's
# violations are folded into the next producer iteration and the loop repeats. At
# the iteration cap, if still failing, the loop emits the best attempt with the
# remaining violations marked NOT PASSING. It never fakes a pass and never prompts
# the human mid-loop.
#
# Usage:
#   taste-loop.sh --task "TASK STRING" --rubric FILE --threshold N --max-iter N [OPTIONS]
#   taste-loop.sh --task-file FILE      --rubric FILE --threshold N --max-iter N [OPTIONS]
#   taste-loop.sh --task-file GOAL --seed DRAFT --rubric FILE --threshold N --max-iter N
#
# Two modes:
#   Produce-from-scratch: the producer writes the first draft from the task.
#   Improve-a-draft (--seed): iteration 1 scores the provided draft directly, and
#     the producer revises it on later iterations. This is the flagship prose path:
#     you hand it an LLM-flavored draft and it grinds until the draft clears the
#     rubric. In seed mode --task is the revision goal, --seed is the first draft.
#
# Options:
#   --task STRING     producer task (produce mode) or revision goal (seed mode)
#   --task-file FILE  read the task from a file instead of --task
#   --seed FILE       existing draft scored as iteration 1 (improve-a-draft mode)
#   --rubric FILE     (required) rubric the reviewer scores against
#   --threshold N     (required) score 0-100 the deliverable must reach to pass
#   --max-iter N      (required) maximum producer/reviewer rounds
#   --out FILE        final deliverable path (default: evidence/final.md)
#   --evidence DIR    evidence directory (default: ./evidence)
#   --max-tokens N    ax token fence per worker (default: 120000)
#   --timeout D       ax timeout per worker (default: 5m)
#   --model M         model for both workers (default: harness default)
#
# On subscription auth --max-cost is inert, so this loop binds each worker with
# --max-tokens. That fence counts input, output, and cache tokens together, so a
# small turn can still read tens of thousands of tokens. Size it as a runaway
# stop, not a precise dial.
#
# The producer emits the deliverable as its final message. The script captures it
# verbatim with `ax result --json` and writes it to a file. The reviewer reads
# that file and the rubric and returns JSON. The producer never touches the
# filesystem. Both workers launch --no-write.
#
# This script is a root entry point. It clears the AX_* session variables so its
# workers start their own fresh run at root depth instead of joining (and
# inheriting the depth fence of) whatever session invoked it. The default fence
# allows root plus one worker level, and these workers are the root level.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TASK=""
TASK_FILE=""
SEED=""
RUBRIC=""
THRESHOLD=""
MAX_ITER=""
OUT=""
EVIDENCE=""
MAX_TOKENS="120000"
TIMEOUT="5m"
MODEL=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --task)       TASK="$2";       shift 2 ;;
    --task-file)  TASK_FILE="$2";  shift 2 ;;
    --seed)       SEED="$2";       shift 2 ;;
    --rubric)     RUBRIC="$2";     shift 2 ;;
    --threshold)  THRESHOLD="$2";  shift 2 ;;
    --max-iter)   MAX_ITER="$2";   shift 2 ;;
    --out)        OUT="$2";        shift 2 ;;
    --evidence)   EVIDENCE="$2";   shift 2 ;;
    --max-tokens) MAX_TOKENS="$2"; shift 2 ;;
    --timeout)    TIMEOUT="$2";    shift 2 ;;
    --model)      MODEL="$2";      shift 2 ;;
    *) echo "taste-loop: unknown flag: $1" >&2; exit 1 ;;
  esac
done

if [[ -n "$TASK_FILE" ]]; then
  [[ -f "$TASK_FILE" ]] || { echo "taste-loop: --task-file not found: $TASK_FILE" >&2; exit 1; }
  TASK="$(cat "$TASK_FILE")"
fi

if [[ -n "$SEED" ]]; then
  [[ -f "$SEED" ]] || { echo "taste-loop: --seed not found: $SEED" >&2; exit 1; }
fi

[[ -n "$TASK" ]]      || { echo "taste-loop: --task or --task-file required" >&2; exit 1; }
[[ -n "$RUBRIC" ]]    || { echo "taste-loop: --rubric required" >&2; exit 1; }
[[ -f "$RUBRIC" ]]    || { echo "taste-loop: rubric not found: $RUBRIC" >&2; exit 1; }
[[ -n "$THRESHOLD" ]] || { echo "taste-loop: --threshold required" >&2; exit 1; }
[[ -n "$MAX_ITER" ]]  || { echo "taste-loop: --max-iter required" >&2; exit 1; }

EVIDENCE="${EVIDENCE:-$PWD/evidence}"
OUT="${OUT:-$EVIDENCE/final.md}"
RUBRIC="$(cd "$(dirname "$RUBRIC")" && pwd)/$(basename "$RUBRIC")"

BEHAVIORS="$SCRIPT_DIR/behaviors"
mkdir -p "$EVIDENCE"

MODEL_ARG=()
[[ -n "$MODEL" ]] && MODEL_ARG=(--model "$MODEL")

# Root entry point: start a fresh run, do not join the caller's.
unset AX_DEPTH AX_MAX_DEPTH AX_RUN AX_GROUP AX_SESSION_ID AX_PARENT AX_LABELS 2>/dev/null || true

echo "[taste] rubric=$RUBRIC threshold=$THRESHOLD max-iter=$MAX_ITER" >&2
echo "[taste] evidence=$EVIDENCE out=$OUT" >&2

# Extract the first {...} JSON object from a worker's final message. Tolerates
# code fences and stray prose around the object.
extract_json() {
  perl -0777 -ne 'print $1 if /(\{.*\})/s'
}

best_score=-1
best_deliverable=""
best_violations="[]"
best_iter=0
final_score=-1
final_pass="false"
converged_iter=0

prev_deliverable=""
violations_json="[]"

# One line per iteration: "iter<TAB>score<TAB>pass". The score trajectory.
: > "$EVIDENCE/trajectory.tsv"

iter=1
while [[ "$iter" -le "$MAX_ITER" ]]; do
  echo "" >&2
  echo "[taste] === iteration $iter/$MAX_ITER ===" >&2

  # ---- PRODUCER ------------------------------------------------------------
  if [[ "$iter" -eq 1 && -n "$SEED" ]]; then
    # Improve-a-draft mode: iteration 1 scores the provided draft directly. No
    # producer launch. The producer runs from iteration 2 onward to revise.
    prev_deliverable="$(cat "$SEED")"
    printf '%s\n' "$prev_deliverable" > "$EVIDENCE/deliverable-iter-$iter.md"
    echo "[taste] iter 1: scoring provided seed draft, producer skipped" >&2
  else
  if [[ "$iter" -eq 1 ]]; then
    PTASK="$TASK

Output only the deliverable text. No preamble, no explanation."
  else
    VIOL_BLOCK="$(printf '%s' "$violations_json" | jq -r '.[] | "- QUOTE: " + .quote + "\n  WHY: " + .why')"
    PTASK="GOAL (unchanged from the first iteration):
$TASK

A reviewer scored your previous deliverable below the threshold. Revise it to
clear every violation. Preserve meaning and factual content. Fix every quoted
span and every other span that fails for the same reason.

--- CURRENT DELIVERABLE ---
$prev_deliverable

--- REVIEWER VIOLATIONS ---
$VIOL_BLOCK

Output only the revised deliverable text. No preamble, no explanation."
  fi

  echo "[taste] launching producer (iter $iter)..." >&2
  PLAUNCH=$(ax claude "$PTASK" \
    --behavior "$BEHAVIORS/producer.md" \
    --label role=producer \
    --no-write \
    --max-tokens "$MAX_TOKENS" \
    --timeout "$TIMEOUT" \
    --wait --unattended --json ${MODEL_ARG[@]+"${MODEL_ARG[@]}"} 2>/dev/null)
  PID=$(printf '%s' "$PLAUNCH" | jq -r .id)
  prev_deliverable=$(ax result "$PID" --json 2>/dev/null | jq -r .result)
  printf '%s\n' "$prev_deliverable" > "$EVIDENCE/deliverable-iter-$iter.md"
  echo "[taste] producer $PID done, $(printf '%s' "$prev_deliverable" | wc -w | tr -d ' ') words" >&2
  fi

  # ---- REVIEWER (fresh each iteration) -------------------------------------
  DELIV_FILE="$EVIDENCE/deliverable-iter-$iter.md"
  RTASK="Score a deliverable against a rubric and return strict JSON.

RUBRIC FILE: $RUBRIC
DELIVERABLE FILE: $DELIV_FILE

Read both files with the Read tool. Score the deliverable against the rubric
only. Quote every offending span. Return only the JSON object defined in your
behavior: {\"score\": int 0-100, \"violations\": [{\"quote\":..., \"why\":...}], \"pass\": bool}.
No prose, no code fences."

  echo "[taste] launching reviewer (iter $iter)..." >&2
  RLAUNCH=$(ax claude "$RTASK" \
    --behavior "$BEHAVIORS/reviewer.md" \
    --label role=reviewer \
    --no-write \
    --max-tokens "$MAX_TOKENS" \
    --timeout "$TIMEOUT" \
    --wait --unattended --json ${MODEL_ARG[@]+"${MODEL_ARG[@]}"} 2>/dev/null)
  RID=$(printf '%s' "$RLAUNCH" | jq -r .id)
  RRAW=$(ax result "$RID" --json 2>/dev/null | jq -r .result)
  VERDICT=$(printf '%s' "$RRAW" | extract_json)

  if [[ -z "$VERDICT" ]] || ! printf '%s' "$VERDICT" | jq empty 2>/dev/null; then
    echo "[taste] reviewer $RID returned unparseable output; recording as score 0" >&2
    printf '%s\n' "$RRAW" > "$EVIDENCE/reviewer-raw-iter-$iter.txt"
    score=0; pass="false"; violations_json='[{"quote":"<none>","why":"reviewer output unparseable"}]'
  else
    printf '%s\n' "$VERDICT" > "$EVIDENCE/verdict-iter-$iter.json"
    score=$(printf '%s' "$VERDICT" | jq -r '.score')
    pass=$(printf '%s' "$VERDICT" | jq -r '.pass')
    violations_json=$(printf '%s' "$VERDICT" | jq -c '.violations')
  fi

  echo "[taste] iter $iter: score=$score pass=$pass violations=$(printf '%s' "$violations_json" | jq 'length')" >&2
  printf '%d\t%s\t%s\n' "$iter" "$score" "$pass" >> "$EVIDENCE/trajectory.tsv"

  # Track best attempt for the cap path.
  if [[ "$score" -gt "$best_score" ]]; then
    best_score="$score"
    best_deliverable="$prev_deliverable"
    best_violations="$violations_json"
    best_iter="$iter"
  fi

  # ---- GATE ----------------------------------------------------------------
  if [[ "$pass" == "true" ]] || [[ "$score" -ge "$THRESHOLD" ]]; then
    final_score="$score"
    final_pass="true"
    converged_iter="$iter"
    printf '%s\n' "$prev_deliverable" > "$OUT"
    echo "" >&2
    echo "[taste] PASS at iteration $iter: score=$score threshold=$THRESHOLD" >&2
    echo "[taste] final deliverable written to $OUT" >&2
    echo "" >&2
    echo "=== SCORE TRAJECTORY ===" >&2
    cat "$EVIDENCE/trajectory.tsv" >&2
    printf '%s\n' "$prev_deliverable"
    exit 0
  fi

  iter=$((iter + 1))
done

# ---- CAP REACHED, STILL FAILING ------------------------------------------
# Emit the best attempt with remaining violations marked NOT PASSING. Never fake
# a pass.
final_score="$best_score"
final_pass="false"

{
  echo "NOT PASSING: reached the iteration cap ($MAX_ITER) without clearing the threshold ($THRESHOLD)."
  echo "Best attempt was iteration $best_iter with score $best_score."
  echo ""
  echo "=== BEST ATTEMPT (iteration $best_iter, score $best_score) ==="
  printf '%s\n' "$best_deliverable"
  echo ""
  echo "=== REMAINING VIOLATIONS ==="
  printf '%s' "$best_violations" | jq -r '.[] | "- QUOTE: " + .quote + "\n  WHY: " + .why'
} > "$OUT"

echo "" >&2
echo "[taste] CAP REACHED after $MAX_ITER iterations, best score=$best_score < threshold=$THRESHOLD" >&2
echo "[taste] emitting best attempt (iter $best_iter) with remaining violations, marked NOT PASSING" >&2
echo "[taste] written to $OUT" >&2
echo "" >&2
echo "=== SCORE TRAJECTORY ===" >&2
cat "$EVIDENCE/trajectory.tsv" >&2

cat "$OUT"
exit 2
