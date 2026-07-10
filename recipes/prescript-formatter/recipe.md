# Recipe: Deterministic pre-script with LLM formatter

A deterministic script collects ground-truth data. The LLM only formats it.
The LLM never fetches, never decides, never hallucinates -- it receives completed
output and produces a human-readable summary or suppresses delivery entirely.

```
[bash check] stdout -> [task string] -> [ax headless formatter] -> [sentinel gate] -> [notify]
```

This pattern also answers the token-efficiency concern: a curl call in bash is
free, while the same call inside an LLM session is billed and subject to tool
permission prompts.

---

## Primitives used

| Primitive | Role |
|---|---|
| `SCRIPT_OUTPUT=$(bash ./check.sh)` | bash string capture (outside ax) |
| `ax claude "TASK" --wait --unattended --json` | headless formatter session |
| `--behavior behaviors/lm-formatter.md` | constrain the LLM to formatting only |
| `ax result "$ID" --json \| jq -r .result` | extract the formatter's output |
| `grep -q '^SILENT$'` | sentinel gate in the wrapper |
| `$NOTIFY_CMD "$OUTPUT"` | delivery (any shell command or binary) |

---

## Folder layout

```
prescript-formatter/
  recipe.md             this file
  run-health-check.sh   wrapper (the one script you run or cron)
  check-services.sh     stub deterministic check (replace with your real check)
  behaviors/
    lm-formatter.md     behavior for the formatter session
```

Copy this directory. Replace `check-services.sh` with your real check. The
wrapper, behavior file, and sentinel gate require no changes across domains.

---

## How to run

```bash
# Default: no-issues path (formatter outputs SILENT, no delivery)
bash run-health-check.sh

# Simulate an outage (formatter outputs an incident summary)
FIXTURE=outage bash run-health-check.sh

# With a real notify target (the wrapper appends the report as one argument)
NOTIFY_CMD="curl -s ntfy.sh/my-alerts -d" bash run-health-check.sh
```

---

## Wrapper walkthrough

```bash
# Stage 1: deterministic check
SCRIPT_OUTPUT=$(bash ./check-services.sh)

# Stage 2: inject into task, launch formatter
TASK="A deterministic health check script ran. Its output is below.
...${SCRIPT_OUTPUT}...
If NO_ISSUES: output SILENT. If OUTAGE_DETECTED: write a summary."

ID=$(ax claude "$TASK" \
    --behavior behaviors/lm-formatter.md \
    --wait --unattended --timeout 5m --max-tokens 200000 \
    --json 2>/dev/null | head -1 | jq -r .id)

# Stage 3: sentinel gate
OUTPUT=$(ax result "$ID" --json | jq -r .result)
echo "$OUTPUT" | grep -q '^SILENT$' && exit 0

# Stage 4: deliver
$NOTIFY_CMD "$OUTPUT"
```

The `--json | head -1 | jq -r .id` pattern extracts the session ID from the JSON
header line printed before `--wait` blocks. The formatter's output is read
cleanly via `ax result --json`.

---

## Behavior file

The formatter behavior is minimal and strict: format only, no commands, no
data gathering.

```markdown
# LLM formatter

You are a formatter only. Your job:

1. Read the check output in your task. Do not run commands. Do not fetch
   additional data. The data was gathered before you ran; it is in your task.
2. Follow the task instructions exactly:
   - NO_ISSUES -> output exactly the word SILENT and stop.
   - OUTAGE_DETECTED -> write a concise incident summary and stop.
     Do not call any tools. Do not run any commands. Just output the summary.
```

---

## Delivery options

The wrapper invokes `$NOTIFY_CMD "$OUTPUT"`: the report arrives as one trailing
argument, so pick a command form that accepts its message that way.

| Option | How |
|---|---|
| Echo (default) | `NOTIFY_CMD="echo '[NOTIFY]'"` |
| ntfy.sh | `NOTIFY_CMD="curl -s ntfy.sh/my-topic -d"` (the report becomes the `-d` body) |
| Slack or other binary | `NOTIFY_CMD="ax-slack"` (any PATH binary taking the message as `$1`) |
| Email | a two-line adapter script: `printf '%s\n' "$1" \| mail -s incident ops@example.com` |

---

## Cron wiring

```
# /etc/cron.d/health-check
*/15 * * * * bash /path/to/run-health-check.sh >> ~/.local/state/ax/log/health.log 2>&1
```

`--wait --unattended` blocks until the formatter session finishes, so the cron
job does not exit early.

---

## Depth fence note

The default depth fence (`--max-depth 1`) allows a root session plus one level
of workers, so the formatter launch works from cron, from a plain shell, and
from inside a root ax session. Only when the wrapper runs inside a session that
is itself a worker (depth 1) would the formatter land at depth 2 and be
refused. In that case relaunch the root with:

```bash
AX_MAX_DEPTH_FLAG="--max-depth 2" bash run-health-check.sh
```

---

## Adapting to other checks

The only per-use-case variables are:
- `check-services.sh`: replace with your real deterministic tool
- The sentinel words in the task string (`NO_ISSUES` / `OUTAGE_DETECTED`): any
  tokens, as long as they match what your check emits
- The summary instructions in the task: adapt to the domain (security audit,
  dependency check, metrics threshold, etc.)

The wrapper, behavior file, and sentinel gate are reusable across every use case.
