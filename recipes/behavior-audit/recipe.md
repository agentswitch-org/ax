# Recipe: Behavior audit

Read recent session transcripts, analyze them for recurring patterns and failure
modes, and produce a curated human-reviewable document of lessons. A headless
worker formats those lessons as proposed edits to a behavior file.

This is not persistent learning. It is a re-derived summary: a worker reads
transcripts at call time and extracts patterns. Every run starts fresh. The output
is a plain markdown file that a human reviews, edits, and optionally applies to
their behavior file. Nothing is applied automatically.

---

## Primitives used

| Primitive | Role |
|---|---|
| `ax search QUERY --json` | content search over all local session transcripts |
| `ax read ID --format text` | pull normalized transcript text for a session |
| `ax list --json` | observe sessions by label, run, or state |
| `ax result ID` | get final report of a concluded session |
| `ax claude - --wait --json` | headless analysis worker, task piped in on stdin |

---

## Folder layout

```
behavior-audit/
  recipe.md   this file
  audit.sh    main script
```

Copy this directory. Run `bash audit.sh` from it. The script works from zero
sessions: if `ax search` returns nothing, it prints a one-line notice and exits
0 without launching a worker.

---

## How to run

```bash
# Basic audit against your session history
bash audit.sh "coordinator behavior"

# A different topic, written to a specific path
bash audit.sh "worker completion verify" completion-patterns.md
```

Arguments:

```
audit.sh [QUERY] [OUT_FILE]

  QUERY     search term (default: "coordinator behavior")
  OUT_FILE  output path (default: audit-output.md)
```

The script takes the top 8 matches and the last 80 transcript lines of each. To
change those bounds, edit the `.ids[:8]` slice and the `tail -80` in `audit.sh`.

---

## What the script does

**Step 1: discover sessions**

```bash
ax search "$QUERY" --json
```

Returns a JSON object with `ids` (ranked list) and `results` (each with `id`,
`title`, and `snippets`). If the list is empty, the script prints a notice and
exits 0 without launching a worker.

**Step 2: pull transcripts**

```bash
ax read "$id" --format text | tail -80
```

For each session, extracts the last 80 lines of the normalized transcript. Appends
to a temp corpus file alongside the session title and matching snippets.

**Step 3: headless analysis worker**

```bash
ID=$(ax claude - --wait --unattended --json < prompt.md | jq -r .id)
ax result "$ID" > audit-output.md
```

`ax claude -` reads the task from stdin, so the whole corpus rides inside the
prompt file. With `--wait` the launch blocks until the worker finishes. The
report is then read with `ax result` (a `--wait` launch keeps stdout clean for
the id line, so redirecting the launch itself would not capture the report).

A single headless worker reads the corpus and produces the structured output:
recurring patterns, failure modes, curated lessons, and proposed behavior file
edits. The worker is instructed to mark single-instance observations as "watch"
not "pattern" and to only cite patterns traceable to the corpus.

The output is a human-reviewable markdown file. Nothing is applied to any
behavior file automatically.

---

## Output format

```markdown
# Behavior audit

> This is a re-derived summary generated at [DATE] by reading [N] sessions.
> It is not persistent memory. A human reviews and applies these findings.

## Recurring successful patterns

### Pattern: [NAME]
Sessions: [id1], [id2], ...
Evidence: "[direct quote]"
Summary: one sentence.

## Recurring failure modes

### Failure: [NAME]
Sessions: [id1], ...
Evidence: "[quote]"
Summary: what went wrong.

## Curated lessons

1. [specific, actionable lesson]

## Proposed behavior file edits

[ADD to section "X"]
> proposed text
Rationale: one sentence from evidence.

[REVISE "Y" to]
> proposed text
Rationale: ...

## Single-instance observations

- [observation] -- session [id]
```

---

## Cron shape

```bash
# Weekly behavioral audit, output to a dated snapshot.
# Note the \% escapes: cron treats an unescaped % as a line break.
0 9 * * 1  cd ~/project && bash behavior-audit/audit.sh "coordinator behavior" \
             ./behaviors/audit-$(date +\%Y\%m\%d).md
```

Dated snapshots accumulate over time. Diff adjacent snapshots to see what changed
in the fleet's behavior week to week.

---

## Depth fence note

`audit.sh` launches a headless worker in step 3. The default depth fence
(`--max-depth 1`) allows a root session plus one level of workers, so the
launch works from a plain shell and from inside a root session. It is refused
only when the script runs inside a session that is already a worker (depth 1),
because the analysis worker would land at depth 2.

Solutions in that case:
- Run `audit.sh` from a shell outside any ax session.
- Launch the root with `--max-depth 2` so its workers may spawn one sub-worker.
- Run step 3 manually: pipe the corpus file through `claude -p` directly
  (headless, no ax tracking needed for a one-off).

---

## Key properties

- Not magic, not automatic. The output is a plain markdown file. A human decides
  what to apply. The behavior file is version-controlled.
- Re-runnable. Run the audit at any time to get a fresh snapshot from current
  session history.
- Auditable. Patterns cite the session IDs they came from.
