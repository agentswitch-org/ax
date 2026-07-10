# Recipe: Taste-gated iteration loop

A producer emits a deliverable. A reviewer scores it against a rubric that
captures your taste. If the score clears a threshold, the loop stops and emits
the deliverable. If it does not, the reviewer's specific violations are folded
into the next producer iteration and the loop repeats, with no human in the
middle. The human sees the result only when it passes, or an honest report when
the loop exhausts its iteration cap.

The flagship use is killing LLM-flavored prose. The rubric encodes what the
reviewer should reject, and the loop grinds a draft until it clears that bar or
reports that it could not.

```
[1] PRODUCER   ax claude "TASK" --behavior behaviors/producer.md --no-write --wait --json
[2] CAPTURE    ax result <id> --json | jq -r .result   -> write to deliverable file
[3] REVIEWER   ax claude "score DELIV against RUBRIC" --behavior behaviors/reviewer.md --no-write --wait --json
[4] PARSE      extract {score, violations, pass} with jq
[5] GATE       score >= threshold or pass -> emit and stop
               else fold violations into [1] and repeat
[6] CAP        at max-iter still failing -> emit best attempt, mark NOT PASSING
```

Stages 2, 4, 5, and 6 are the loop's mechanism. Stages 1 and 3 are the two
workers. Only the rubric and the task change between use cases.

---

## When to use this pattern

- A deliverable has a quality bar that a human keeps re-applying by hand.
- The bar can be written as a rubric a reviewer scores against.
- Iterating on specific critique converges. Prose, config, schemas, and docs fit.
- You want the human surfaced only on success or on an honest cap report, not on
  every draft.

If the bar cannot be written down, this loop cannot run. The rubric is the taste.

---

## Files

| File | Purpose |
|---|---|
| `recipe.md` | this file |
| `taste-loop.sh` | the loop: producer, capture, reviewer, parse, gate, cap |
| `behaviors/producer.md` | producer role: emit or revise the deliverable |
| `behaviors/reviewer.md` | reviewer role: score against the rubric, return strict JSON |
| `rubrics/prose-flat-register.md` | flagship rubric: flat developer-doc register |
| `rubrics/TEMPLATE.md` | blank rubric skeleton for writing your own taste |

The flagship proof inputs (the revision goal, the LLM-flavored seed draft, the
contradictory rubric, and the recorded convergence runs) live outside the bundle
in `tests/recipes/taste-gate/`. They demonstrate and re-verify the recipe; they
are not needed to run it against your own material.

---

## Primitives used

| Primitive | Role |
|---|---|
| `ax claude "TASK" --wait --unattended --json` | headless worker that runs to completion |
| `--behavior behaviors/producer.md` | constrain the producer to emit only the deliverable |
| `--behavior behaviors/reviewer.md` | constrain the reviewer to a strict JSON verdict |
| `--no-write` | both workers run under the capability fence with zero write scope, so they emit only through their result |
| `ax result <id> --json` | read the worker's final message as `.result` |
| `jq` | parse the reviewer verdict and format violations for the next producer |
| `--max-tokens N` | the fence that binds on subscription auth |

---

## Two modes

**Produce-from-scratch.** The producer writes the first draft from the task, the
reviewer scores it, and the loop iterates. Use `--task` or `--task-file`.

**Improve-a-draft (`--seed`).** Iteration 1 scores an existing draft directly,
with no producer launch. The producer runs from iteration 2 onward to revise
against the violations. This is the flagship prose path. You hand it an
LLM-flavored draft and it grinds the draft until it clears the rubric. In seed
mode `--task` carries the revision goal and `--seed` carries the first draft.

## The two workers

**Producer.** Emits the deliverable as its final message. On the first iteration
(produce mode) it gets the goal and the source material. On later iterations it
gets the current deliverable and the reviewer's violations, and it revises. It
never touches the filesystem. The loop captures its final message verbatim and
writes it to a file. In seed mode the producer is skipped on iteration 1.

**Reviewer.** Launched fresh each iteration with no memory of prior rounds. Reads
the rubric file and the deliverable file. Scores against the rubric only. Returns
one JSON object:

```json
{
  "score": 0,
  "violations": [{"quote": "exact offending text", "why": "reason, tied to a rubric item"}],
  "pass": false
}
```

The reviewer must quote real offending text, score against the rubric only, and
never rubber-stamp. A surviving heavy pattern forces `pass` false.

---

## How the loop works

The deliverable moves by file, the verdict moves by JSON.

1. The producer emits the deliverable as its final message. The loop reads it
   with `ax result --json | jq -r .result` and writes it to
   `evidence/deliverable-iter-N.md`. The producer needs no write access.
2. The reviewer reads that file and the rubric and returns JSON. The loop
   extracts the object (tolerating code fences and stray prose), then reads
   `score`, `pass`, and `violations` with `jq`.
3. Gate. If `pass` is true or `score` is at or above the threshold, the loop
   writes the deliverable to the output path and stops.
4. Otherwise the loop formats the violations into the next producer task and
   repeats. The producer sees each violation as a quoted span and a reason.
5. At the iteration cap, if still failing, the loop emits the best-scoring
   attempt with its remaining violations, marked NOT PASSING. It never writes a
   passing verdict it did not earn.

The score trajectory is recorded to `evidence/trajectory.tsv`, one line per
iteration as `iter<TAB>score<TAB>pass`.

---

## How to run

```bash
# Flagship: grind an existing LLM-flavored draft to a flat one (seed mode).
# The proof inputs live in tests/recipes/taste-gate/ (outside the bundle).
bash taste-loop.sh \
  --task-file ../../tests/recipes/taste-gate/prose-goal.md \
  --seed ../../tests/recipes/taste-gate/llmy-paragraph.md \
  --rubric rubrics/prose-flat-register.md \
  --threshold 85 \
  --max-iter 5

# Produce-from-scratch: the producer writes the first draft.
bash taste-loop.sh \
  --task "Rewrite this README intro in flat developer-doc register: <paste>" \
  --rubric rubrics/prose-flat-register.md \
  --threshold 85 \
  --max-iter 4
```

The final deliverable prints to stdout and is written to `--out` (default
`evidence/final.md`). Exit code 0 means it passed. Exit code 2 means it hit the
cap without passing, and the output carries the best attempt plus its remaining
violations.

---

## Flags

```
taste-loop.sh --task "TASK" --rubric FILE --threshold N --max-iter N [OPTIONS]

  --task STRING     producer task (produce mode) or revision goal (seed mode)
  --task-file FILE  read the task from a file instead of --task
  --seed FILE       existing draft scored as iteration 1 (improve-a-draft mode)
  --rubric FILE     (required) rubric the reviewer scores against
  --threshold N     (required) score 0-100 the deliverable must reach to pass
  --max-iter N      (required) maximum producer/reviewer rounds
  --out FILE        final deliverable path (default: evidence/final.md)
  --evidence DIR    evidence directory (default: ./evidence)
  --max-tokens N    ax token fence per worker (default: 120000)
  --timeout D       ax timeout per worker (default: 5m)
  --model M         model for both workers (default: harness default)
```

---

## Fences and depth

```bash
ax claude "TASK" \
  --behavior behaviors/producer.md \
  --no-write \
  --max-tokens 120000 \
  --timeout 5m \
  --wait --unattended --json
```

On subscription auth `--max-cost` is inert, so each worker is bound with
`--max-tokens`. That fence counts input, output, and cache read and write tokens
together, and cache traffic dominates, so a single small turn can read tens of
thousands of tokens by this measure. Size it as a runaway stop, not a precise
dial.

`taste-loop.sh` is a root entry point. It clears the `AX_*` session variables so
its workers start their own fresh run at root depth instead of joining, and
inheriting the depth fence of, whatever session invoked it. The default fence
allows root plus one worker level. These workers are the root level, so the loop
runs from a plain shell or from inside a session without hitting the depth fence.

---

## Writing your own rubric

Copy `rubrics/TEMPLATE.md`. The rubric is where the taste lives. State what to
penalize as patterns the reviewer can quote and tie to a numbered item. State
what to reward as observable properties of the text. Give per-pattern weights and
a scoring floor so the reviewer calibrates. Set a threshold that forces out every
heavy pattern. A vague rubric produces vague violations and the loop stops
converging.

`rubrics/prose-flat-register.md` is a worked example for LLM-flavored writing.
It penalizes rhetorical openers, marketing verbs, cutesy endings, cutesy
imperatives, word salad, filler intensifiers, em-dashes, semicolons, and
rule-of-three padding, and it targets the register of FFmpeg and CUDA sample
docs.

---

## Extending the loop

- **Panel of reviewers.** Launch N reviewers per iteration against different
  rubrics (correctness, style, safety) and gate on all of them. Fold the union of
  violations into the next producer.
- **Rubric ensemble.** Score one deliverable against several rubrics and require
  each to clear its own threshold.
- **Two-stage gate.** A cheap reviewer screens, an expensive reviewer confirms
  only the drafts that pass the screen.
- **Human as final gate.** Keep the loop autonomous to the threshold, then
  surface the passing draft for a human accept step. The loop still spends no
  human attention on the failing drafts.
