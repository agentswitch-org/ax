# Recipe: Scattered sources to queryable vault

Fan N workers in parallel over a corpus. Each worker extracts structured notes
into a shared vault directory. A verify wave checks completeness. The vault is
the queryable output: any future ax session can draw from it with `--dir vault/`.

---

## When to use this pattern

- A corpus is too large for one context window.
- Each item (correspondent, document, page) maps naturally to one worker.
- You want a persistent, queryable knowledge base built from scattered sources.
- Provenance matters: each vault note is traceable to the worker that wrote it.

---

## Primitives used

| Primitive | Role |
|---|---|
| `ax claude "TASK" --wait --json ... &` | launch worker headlessly, in background |
| `wait "${PIDS[@]}"` | barrier: wait for all background workers |
| `ax list --json --run RUN_ID` | snapshot run tree (proves parallelism) |
| `--run RUN_ID` | groups all workers and the verify pass into one run |
| `--label key=value` | annotates workers for filtering and auditing |
| `--max-workers N` | caps concurrent sessions |

---

## Folder layout

```
vault-fanout/
  recipe.md         this file
  fanout.sh         fan out over chunks, then one verify pass
  check-vault.sh    accept check: every notes file has the required sections
  split-mbox.sh     split an mbox into per-correspondent chunks
  fixture/          you create this before running
    synthetic.mbox
    chunks/
      chunk-alice.txt
      chunk-bob.txt
      chunk-carol.txt
  vault/            written by extract workers
    notes-alice.md
    notes-bob.md
    notes-carol.md
```

Copy this directory, prepare the chunks, then run `bash fanout.sh`. For a demo
corpus, the generator lives outside the bundle in
`tests/recipes/vault-fanout/gen-mbox.sh`:

```bash
bash ../../tests/recipes/vault-fanout/gen-mbox.sh > fixture/synthetic.mbox
bash split-mbox.sh fixture/synthetic.mbox fixture/chunks
bash fanout.sh
```

For your own data, write a chunker that emits `fixture/chunks/chunk-SLUG.txt`
and skip the generator. No real corpus is required to try the recipe.

---

## How it works

```
fanout.sh   (chunks already in fixture/chunks/)

  [1] fan out: launch N workers in parallel
      for each chunk:
        ax claude "extract notes -> vault/notes-SLUG.md" --wait --json ... &

  ALL N workers now running before any finishes.

  [2] wait for all workers     (wait "${PIDS[@]}")

  [3] verify wave: one worker reads all vault notes, launched with
      --accept ./check-vault.sh so the run concludes only if the check passes
```

Parallelism proof: every worker is launched with `&` before any is awaited, and
`ax list --json --run "$RUN_ID"` taken mid-run shows every extract worker live
at once.

---

## How to run

```bash
cd vault-fanout/
bash ../../tests/recipes/vault-fanout/gen-mbox.sh > fixture/synthetic.mbox
bash split-mbox.sh fixture/synthetic.mbox fixture/chunks
bash fanout.sh
```

`fanout.sh` derives its own run id (`vault-$(date +%s)`). To pin one, edit
`RUN_ID` at the top of the script.

---

## Core fan-out pattern

```bash
RUN_ID="vault-$(date +%s)"
PIDS=()

for CHUNK in fixture/chunks/chunk-*.txt; do
  SLUG="${CHUNK##*/chunk-}"; SLUG="${SLUG%.txt}"

  ax claude "Read $CHUNK. Extract key topics, commitments, and dates.
             Write notes to vault/notes-${SLUG}.md." \
    --run "$RUN_ID" --label "role=worker" --label "correspondent=$SLUG" \
    --max-workers 5 --max-tokens 500000 --timeout 10m \
    --wait --json > "/tmp/launch-${SLUG}.json" &

  PIDS+=($!)
done

# Barrier: wait for all workers before proceeding to verify.
wait "${PIDS[@]}"
```

The worker gets the chunk path, not the inlined text, so a large chunk does not
bloat the task string. For a big corpus, add `--model haiku` to the extract
workers (mechanical note-taking) and keep the default model for the verify pass.

Workers launch in the background with `&`. All N are live before any finishes.
The `wait` builtin is the barrier. No ax-specific barrier primitive is needed.

---

## Verify wave

After all extract workers finish, one verify worker reads the entire vault. The
`--accept ./check-vault.sh` check is the authority: the run concludes only if
every notes file passes.

```bash
ax claude "Check each file in vault/notes-*.md for required sections.
    Report PASS or FAIL per file. Exit 0 only if all pass." \
    --run "$RUN_ID" --label role=verifier \
    --wait --accept ./check-vault.sh
```

`check-vault.sh` reads each `vault/notes-*.md` and confirms it carries every
required section (topics, commitments, dates). Edit its `REQUIRED` list to match
the fields your extract workers are told to write. The worker's own report is
advisory; the check is what gates the run.

---

## Querying the vault

Once built, any session can query it:

```bash
# Interactive query
ax claude "Which correspondents mentioned a contract renewal?" --dir vault/

# Headless one-shot
ax claude "Summarize all commitments from Alice in 2026." --dir vault/ --wait
```

The vault is plain markdown and works with any ax harness.

---

## Fences

| Flag | Value | Why |
|---|---|---|
| `--max-workers 5` | N+2 | N extract + 1 verify + 1 headroom |
| `--max-tokens 500000` | 500k | whole-run token cap, the fence that binds on subscription auth. Counts input, output, and cache read/write together, so budget tens of thousands per worker |
| `--timeout 10m` | 10m | per-launch wall-clock cap so hung workers do not block forever |

On API auth (`--api`), add `--max-cost` in USD for a hard-dollar cap; on the
default subscription auth it is inert. The default depth fence (root plus one
worker level) already covers this fan-out from a plain shell, so no `--max-depth`
is needed.

---

## Adapting to other sources

| Source | Chunk strategy | Worker task shape |
|---|---|---|
| mbox (email export) | per correspondent | extract contacts, topics, commitments |
| Notion export | per page | extract title, owner, key points, open items |
| Obsidian vault | per note | reformat, add back-links, tag |
| Slack export | per channel | extract decisions, action items, links |
| PDF archive | per document | extract abstract, key claims, citations |

Only `split-mbox.sh` (the source-specific chunker) and the worker task string
change across use cases. The `fanout.sh` pattern is the same.
