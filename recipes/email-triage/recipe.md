# Recipe: Email triage with sentinel gate

Process a local maildir, take an action on every message, and notify only when
something requires human attention. The default is silence. This is the opposite
of a daily digest: the recipe acts on everything and surfaces only what cannot
be handled automatically.

---

## Primitives used

| Primitive | Role |
|---|---|
| `ax claude "TASK" --wait --unattended` | headless triage worker |
| `ax result <id>` | read the worker's output after it exits |
| `--behavior behaviors/email-triage.md` | constrain the worker to triage-only output |
| `grep -q '^SILENT$'` | sentinel gate in the wrapper |
| `$NOTIFY_CMD "$SUMMARY"` | delivery (any shell command or binary) |

---

## Folder layout

```
email-triage/
  recipe.md                  this file
  triage-wrapper.sh          main script: build task, run worker, dispatch, gate, notify
  behaviors/
    email-triage.md          worker behavior: classify, act, sentinel
```

Copy this directory. Adapt `ARCHIVE_CMD`, `UNSUBSCRIBE_CMD`, `DRAFT_REPLY_CMD`,
and `NOTIFY_CMD` to real adapters. The wrapper's shell logic and sentinel gate
require no changes.

---

## How it works

```
[1] TRIGGER
    cron: 0 */4 * * * bash /path/to/triage-wrapper.sh ~/Maildir >> ~/log/email-triage.log

[2] WORKER
    ax claude "TASK" --behavior behaviors/email-triage.md --model haiku
      --wait --unattended --timeout 3m --max-cost 0.25 --max-tokens 200000

[3] ACTION DISPATCH
    parse ACTION|||ARCHIVE|||, ACTION|||UNSUBSCRIBE|||, ACTION|||DRAFT_REPLY||| lines
    fields separated by ||| so maildir filenames containing ":" are preserved intact
    call archive/unsubscribe/draft-reply adapters for each

[4] SENTINEL GATE
    grep -q '^SILENT$' <<< "$TRIAGE_OUTPUT" && exit 0

[5] NOTIFY (only when gate does NOT hold)
    "$NOTIFY_CMD" "$SUMMARY"
```

The worker decides what each email is and what action to take. The wrapper owns
the notification decision: a single `grep`.

---

## The inverted default

Most cron-shaped recipes notify on completion. This one inverts that: the normal
exit is silence. The `SILENT` sentinel suppresses everything. Only the absence
of `SILENT` triggers delivery. This is the act-on-everything, notify-on-exception
pattern.

Two ways to wire the sentinel:

```bash
# Option A: sentinel suppresses the notify call (used in triage-wrapper.sh)
grep -q '^SILENT$' <<< "$output" && exit 0
"$NOTIFY_CMD" "$SUMMARY"

# Option B: sentinel suppresses ax's own run-success hook
# The worker emits SILENT and does NOT call ax tag --outcome success.
# The [notify] run-success hook never fires on a silent run.
```

Both work. Option A keeps the notify target in bash (more portable). Option B
wires delivery through `~/.config/ax/config.toml`, letting you change the target
without touching the wrapper.

---

## How to run

```bash
# Production: pass your real maildir
bash triage-wrapper.sh ~/Maildir

# Override adapters via env (each receives its argument(s) appended)
ARCHIVE_CMD="./adapters/archive.sh" \
NOTIFY_CMD="curl -s ntfy.sh/my-topic -d" \
bash triage-wrapper.sh ~/Maildir
```

Cron entry:

```cron
# Run every 4 hours
0 */4 * * * bash /path/to/triage-wrapper.sh ~/Maildir >> ~/.local/state/ax/log/email-triage.log 2>&1

# Near-real-time with a polling mail fetcher
*/15 * * * * mbsync -a && bash /path/to/triage-wrapper.sh ~/Maildir >> ~/.local/state/ax/log/email-triage.log 2>&1
```

---

## Wiring real adapters

Replace the stub defaults with real commands via env vars. The wrapper appends
the message filename (and for DRAFT_REPLY, the reply text) as arguments, so
each adapter must accept its input as arguments, not stdin:

```bash
export ARCHIVE_CMD="./adapters/archive.sh"          # receives: <filename>
export UNSUBSCRIBE_CMD="./adapters/unsubscribe.sh"  # receives: <filename>
export DRAFT_REPLY_CMD="./adapters/draft-reply.sh"  # receives: <filename> <reply text>
export NOTIFY_CMD="curl -s ntfy.sh/my-topic -d"     # receives: <summary>
```

An archive adapter for the [himalaya](https://github.com/pimalaya/himalaya)
CLI, whose Maildir message ids are the message filename ids and whose move
syntax is `himalaya message move <id> <folder>`, is two lines:

```bash
#!/usr/bin/env bash
himalaya message move "$(basename "$1")" Archive
```

Drafting replies is harder to automate: `himalaya message reply` opens an
editor, so a scriptable reply adapter needs your mail tool's non-interactive
compose path (or can simply write the draft text to a drafts folder for you to
finish by hand). Start with the stub and keep drafts human-reviewed.

Or wire through the ax notify hook in `~/.config/ax/config.toml`:

```toml
[notify]
run-success = "curl -s ntfy.sh/my-topic -d '{summary}'"
```

With Option B, have the worker call `ax tag "$AX_SESSION_ID" --outcome success`
when important items need attention (instead of printing `SILENT`). The sentinel
gate then lives inside the worker's behavior, not the bash wrapper.

---

## Fences

The worker runs with `--model haiku`, `--max-tokens 200000`, `--max-cost 0.25`,
and `--timeout 3m`. On the default subscription auth there is no per-token cost
and `--max-cost` is inert, so `--max-tokens` is the fence that binds (it counts
input, output, and cache read/write together). `--max-cost` matters only if you
opt in to API billing with `--api`, where a 50-message haiku batch typically
costs a few cents. For large inboxes, chunk the maildir: process N messages per
cron invocation and track the watermark in a state file.
