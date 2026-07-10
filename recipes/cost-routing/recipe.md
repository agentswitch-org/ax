# Recipe: Cost-tiered model routing with escalation

Run a task on the cheapest model tier first. If the accept check fails, escalate
to the next tier automatically. Cap spend with token fences. Read cost telemetry
from run records.

Five ax primitives compose the whole pattern: `--model`, `--wait`, `--accept`,
`--max-tokens`, and the run record at `~/.local/state/ax/runs/<run-id>.json`.

---

## Primitives used

| Primitive | Role |
|---|---|
| `ax claude "TASK" --model M` | pick the tier |
| `--wait` | blocking headless run, exit 0 on success and non-zero on failure |
| `--accept "bash ./check.sh"` | mechanical accept check that decides the `--wait` exit code |
| `--max-tokens N` | spend fence per attempt |
| `~/.local/state/ax/runs/<id>.json` | cost telemetry per run |

---

## Folder layout

```
cost-routing/
  escalate.sh   wrapper that tries each tier in order
  check.sh      sample accept check (replace with your task's check)
  recipe.md     this file
```

Copy this directory. Replace `check.sh` with a check specific to your task. The
rest requires no changes for most use cases.

---

## How it works

```
bash escalate.sh "TASK"

  for MODEL in haiku sonnet opus:
    ax claude "$TASK" --model "$MODEL" --max-tokens 100000 --wait --accept "bash ./check.sh"
    exit 0 -> done, stop
    exit N -> escalate to the next tier

  every tier failed -> exit 1
```

With `--wait`, the `--accept "bash ./check.sh"` check is the authority on the exit
code: after the headless session finishes, the `--wait` process runs the check
itself (in the launch directory). Check exits 0, `--wait` exits 0. Check exits
non-zero, `--wait` exits 1 and prints the check output as "not done". The check
also guards the outcome tag: if the session calls `ax tag --outcome success`
mid-run, ax runs the same check and rejects the tag on a non-zero exit. The
router treats any non-zero exit from `--wait` as "escalate."

---

## How to run

```bash
# Copy and adapt check.sh for your task, then:
bash escalate.sh "your task description" "bash ./check.sh"
echo "exit: $?"
```

From a coordinator session:

```bash
bash escalate.sh "summarize the latest 10 issues from $REPO" "bash ./check.sh"
```

---

## Writing the accept check

The check is the definition of "done." Write it to inspect the artifact directly.

```bash
#!/usr/bin/env bash
# check.sh - exits 0 when the task's output meets the acceptance criteria
OUTPUT="${OUTPUT_FILE:-./output.json}"
if [[ ! -f "$OUTPUT" ]]; then exit 1; fi
python3 -c "
import json, sys
d = json.load(open('$OUTPUT'))
assert d.get('status') == 'ok', 'bad status'
assert len(d.get('result', '')) > 0, 'empty result'
print('OK')
"
```

The check does not need to know which tier ran. It only inspects the artifact.

---

## Tier configuration

| Tier | Model | Use case | Fence |
|---|---|---|---|
| cheap | `haiku` | mechanical tasks, extraction, formatting | `--max-tokens 100000` |
| standard | `sonnet` | reasoning, multi-step tasks | `--max-tokens 100000` |
| premium | `opus` | hard problems, taste-critical decisions | `--max-tokens 100000` |

`escalate.sh` fences every tier at `--max-tokens 100000`. Raise the cap for the
premium tier, or give each tier its own budget, by editing the loop. The short
model names resolve to the current haiku, sonnet, and opus releases.

On subscription auth (the default), `--max-cost` is inert. Fence on
`--max-tokens` instead. On API auth (`--api`), swap to `--max-cost` in USD.

`--max-tokens` counts input, output, and cache read/write tokens across the
run. Cache traffic dominates: a single small claude turn can consume 20-30k
tokens by this measure, so size budgets in the tens of thousands, not hundreds.

---

## Cost telemetry

After each attempt, the run record is written to:

```
~/.local/state/ax/runs/<run-id>.json
```

Fields of interest:

```json
{
  "group": "<run-id>",
  "cost": 0.0763,
  "tokens": { "in": 5502, "out": 9, "cache_read": 15874, "cache_write": 6510 },
  "outcome": "success"
}
```

Spend summary across all runs:

```bash
for f in ~/.local/state/ax/runs/*.json; do
  jq -r '[.group, .outcome, .cost] | @tsv' "$f"
done | column -t
```

Live status during a run, per session (`outcome` lives in the run record and
`ax result`, not in `ax list`):

```bash
ax list --json --run "$AX_RUN" | jq '.sessions[] | {model, cost, state, done, failed}'
```

---

## Rate-limit fallback

Rate limits produce a crashed or non-zero `--wait` exit. No special handling is
needed: the router escalates on any non-zero exit. To distinguish a rate limit
from a task failure, inspect the run record `outcome` field:

```bash
jq -r .outcome ~/.local/state/ax/runs/<id>.json
# "crashed"    -> likely rate limit or auth issue
# "gave_up"   -> session tried but could not complete
# "budget_hit" -> fence tripped
```

---

## Optional: policy fence

Add a model policy in `~/.config/ax/config.toml` to block accidental premium
runs outside the escalation wrapper:

```toml
[policy]
model = { deny = ["*opus*"] }
```

The escalation wrapper becomes the only authorized path to opus. Any other
session attempting it is refused by ax.

---

## Verification status

The escalation logic (tier order, exit-code contract, accept-check gating) was
tested with stub tier commands that mirror the `ax claude --wait --accept` exit
contract, without spending tokens. The launch flags in `escalate.sh` are
verified against the current ax CLI. The full loop with real model attempts has
not been exercised end to end, so expect to tune the `--max-tokens` budgets
for your task the first time you run it for real.

---

## Key properties

- No new ax code. Five flags already shipped.
- No daemon. Each tier attempt is a blocking shell command.
- Composable. Drop `escalate.sh` into any cron, coordinator, or pipeline.
- Observable. Run records carry cost and outcome for every attempt.
