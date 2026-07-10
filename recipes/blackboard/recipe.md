# Recipe: Cross-agent blackboard

Two or more workers coordinate through a shared JSON file on disk. Workers read
from it, append to it, and leave it as the durable record of the exchange. The
coordinator sequences workers and uses ax verbs to observe and steer.

This is the ax answer to multi-agent message-bus workflows. No queue server, no
pub-sub daemon: a plain JSON file and five ax verbs.

---

## When to use this pattern

- Two agents need to build on each other's output without sharing context windows.
- You want a persistent, inspectable record of what each agent decided.
- A producer generates candidates and a critic reviews them without access to the
  producer's reasoning, only its conclusions.
- More than two passes are needed: multiple critique rounds, a tie-break judge,
  or an append-only audit log of agent decisions.

---

## Primitives used

| Primitive | Role |
|---|---|
| shared file on disk | the blackboard, read and written by all agents |
| `ax claude "TASK" --wait --json` | launch a headless worker |
| `ax wait <id> --timeout T` | block until the worker exits |
| `ax result <id>` | read the worker's final report after it exits |
| `ax read --run "$AX_RUN" --follow --exclude "$AX_SESSION_ID"` | stream events across all workers (interactive variant) |
| `ax send <id> "..."` | steer a live worker (interactive variant) |
| `ax list --json --run "$AX_RUN"` | snapshot all workers in the run |

---

## Folder layout

```
blackboard/
  recipe.md   this file
  run.sh      headless pipeline script (Variant A)
```

Copy the directory and run `bash run.sh`. The script initializes a fresh
blackboard inline with `echo '{"items":[],"verdicts":[]}' > "$BB"`, so there is
nothing else to seed. Both workers get their role from an inline task string, so
the recipe needs no behavior files. No prior sessions or data required.

---

## Two variants

### Variant A: headless pipeline (recommended starting point)

The coordinator script sequences workers: producer finishes, coordinator reads
the blackboard, coordinator launches the critic. No live message passing. Fully
runnable without tmux.

```
coordinator (script)
  producer  --wait         # writes item to blackboard
  [wait]                   # coordinator reads blackboard
  critic    --wait         # reads blackboard, writes verdict
  [verify]                 # coordinator reads final blackboard
```

### Variant B: interactive bus

Both workers run interactively (no `--wait`). The coordinator uses
`ax read --run "$AX_RUN" --follow --exclude "$AX_SESSION_ID"` as a notification
bus. When the producer's exit event fires, the coordinator checks the blackboard
and uses `ax send` to signal the critic. Use this variant when workers are
long-running or need mid-task steering.

```
coordinator (interactive session, --no-write)
  producer  (interactive)
  critic    (interactive)
  ax read --run "$AX_RUN" --follow --limit 1 --exclude "$AX_SESSION_ID"
  on producer exit: verify blackboard, ax send critic
  on critic exit:   verify blackboard, ax tag --outcome success
```

Note: `ax send` requires the target session to be live in a tmux window.
Headless `--wait` workers cannot receive `ax send`. Use Variant A for pure
headless runs.

---

## How to run (Variant A)

```bash
bash run.sh
```

Or point it at a specific path:

```bash
BB=/tmp/my-run-blackboard.json bash run.sh
```

The script initializes a fresh blackboard at `/tmp/blackboard.json` (override
with `BB`), runs the full producer-critic round, and prints the final board with
`jq`.

---

## Blackboard schema

```json
{
  "items": [
    {
      "role": "producer",
      "claim": "...",
      "confidence": 0.97
    }
  ],
  "verdicts": [
    {
      "role": "critic",
      "ref": "<claim text mirrored from item>",
      "verdict": "<one-sentence factual assessment>",
      "factual": true,
      "correct": true,
      "confidence": 0.98
    }
  ]
}
```

The schema is yours to define. Keep writes append-only: each agent adds to its
own array and never modifies what another agent wrote. This makes the blackboard
a tamper-evident audit log.

---

## Fences

```bash
# Headless pipeline (Variant A)
ax claude "TASK" \
  --max-tokens 100000 \
  --timeout 5m \
  --wait

# Interactive run with coordinator (Variant B)
ax claude "coordinator task" \
  --behavior ../../behaviors/coordinator.md \
  --no-write \
  --max-workers 3 \
  --max-depth 1 \
  --max-tokens 500000 \
  --timeout 20m
```

On subscription (no API key), `--max-cost` is inert. Use `--max-tokens` to cap
consumption across the run. Note that `--max-tokens` counts input, output, and
cache read/write tokens together, and cache traffic dominates: a single small
claude turn can consume 20-30k tokens by this measure. A tripped fence
cascade-kills the run, so size the budget generously and treat it as a runaway
stop, not a precision dial.

---

## Extending beyond two agents

- **Multi-round critique:** run the critic loop N times. Each round reads the
  previous verdicts and refines.
- **Judge tier:** after producer and critic, launch a judge worker that reads
  both `items` and `verdicts` and writes a `judgment` array.
- **Fan-out critique:** launch N critic workers in parallel, each writing to a
  separate key (e.g. `verdicts_factual`, `verdicts_style`, `verdicts_safety`).
  The coordinator joins by checking all keys are populated.
- **Backlog drain:** the producer writes N items, the coordinator launches one
  critic per item (or batches them) and joins on all critics. The blackboard is
  the queue.

All extend the same pattern: append-only writes, role-keyed arrays, each agent
reads the whole board but writes only to its own section.
