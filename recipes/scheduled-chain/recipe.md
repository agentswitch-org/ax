# Recipe: Scheduled chain

The canonical scheduled automation shape. A cron trigger calls a wrapper that
runs an ax worker, checks the output for a sentinel word, and delivers a report
only when there is something actionable.

```
[1] TRIGGER     cron, systemd.timer, or launchd calls bash ax-scheduled-chain.sh
[2] WORKER      ax claude "TASK" --behavior behaviors/worker.md --wait --unattended
[3] GATE        grep -q "^SILENT$" in the wrapper; exit 0 to suppress delivery
[4] NOTIFY      call the notify command, or the ax run-success hook fires
```

Nothing in stages 2-4 is workflow-specific. Only the task string in stage 1
changes.

---

## Files

| File | Purpose |
|---|---|
| `behaviors/worker.md` | shared worker behavior: gather, summarize, sentinel gate |
| `ax-scheduled-chain.sh` | parametrized wrapper, swap `--task` to change the workflow |
| `notify-stub.sh` | logging notify adapter (replace with ax-slack, ntfy, etc.) |

---

## Primitives used

| Primitive | Role |
|---|---|
| `ax claude "TASK" --wait --unattended` | headless worker that runs to completion |
| `--behavior behaviors/worker.md` | constrain the worker to the scheduled-chain role |
| `ax result "$SESSION_ID"` | read the worker's final output |
| `grep -q "^SENTINEL$"` | sentinel gate that suppresses delivery on clean runs |
| `$NOTIFY_CMD "$OUTPUT"` | delivery target (any shell command string) |

---

## How to run

```bash
# Daily briefing
bash ax-scheduled-chain.sh \
  --task "Search the web for the top 5 AI agent developments from yesterday. Summarize each in 2-3 sentences." \
  --notify "bash ./notify-stub.sh" \
  --max-cost 2.00 \
  --timeout 10m

# Dependency security audit (pre-script pattern)
VULN_OUTPUT=$(govulncheck ./... 2>&1)
bash ax-scheduled-chain.sh \
  --task "A Go dependency vulnerability scan produced:

$VULN_OUTPUT

Triage the findings. If the scan reports no vulnerabilities affecting the
code (the exact wording varies by govulncheck version), output exactly SILENT
on its own line and stop. Otherwise summarize each finding:
vulnerability ID, affected module, version found, fixed version, recommended
upgrade action." \
  --notify "bash ./notify-stub.sh" \
  --max-cost 0.50 \
  --timeout 5m
```

---

## Cron entries

```
# Daily briefing on weekdays at 8am
0 8 * * 1-5  AX_CHAIN_NOTIFY=/usr/local/bin/ax-slack \
             bash /path/to/ax-scheduled-chain.sh \
             --task "Search the web for top 5 AI agent developments from yesterday. Summarize each." \
             >> ~/.local/state/ax/log/briefing.log 2>&1

# Weekly vulnerability audit on Monday at 2am
0 2 * * 1  /path/to/run-audit.sh >> ~/.local/state/ax/log/audit.log 2>&1
```

---

## Wrapper flags

```
bash ax-scheduled-chain.sh --task "TASK STRING" [OPTIONS]

  --task TASK       (required) task string; carries all workflow context
  --run-id ID       run id label (default: chain-YYYYMMDD-HHMMSS)
  --notify CMD      notify command; receives report as first argument
  --sentinel WORD   sentinel word that suppresses delivery (default: SILENT)
  --behavior FILE   behavior file path (default: behaviors/worker.md)
  --max-cost N      ax cost fence in USD (default: 2.00; binds only with API
                    auth, inert on the default subscription auth)
  --max-tokens N    ax token fence (default: 400000); the fence that binds on
                    subscription auth. Counts input, output, and cache tokens
  --timeout D       ax timeout duration (default: 10m)
```

All flags also accept `AX_CHAIN_*` env vars (useful in crontab).

---

## Notify hook alternative

Instead of `--notify`, configure the ax notify hook for automatic delivery:

```toml
# ~/.config/ax/config.toml
[notify]
run-success = "ax-slack '{summary}'"
```

The behavior file (`behaviors/worker.md`) tells the worker to call
`ax tag --outcome success` only when it has an actionable report, and to stop
after emitting `SILENT` otherwise. The `run-success` hook fires only from that
tag call, so it stays quiet on suppressed runs. No extra logic needed.

---

## Workflow catalog

Every row below uses the same wrapper and behavior file. Only `--task` changes.

| Workflow | Task string (core) | Silent when |
|---|---|---|
| Daily briefing | "Search the web for top 5 AI agent developments from yesterday. Summarize each in 2-3 sentences." | never (always delivers) |
| Dependency audit | "A Go vuln scan produced: $VULN_OUTPUT\nTriage. If clean output SILENT." | no vulnerabilities |
| Nightly backlog triage | "Run: gh issue list --state open --json ... then categorize. If no new issues since yesterday, output SILENT." | zero new issues |
| Docs drift detection | "Run: gh pr list --merged --limit 20 and compare touched files against the documentation. If they match, output SILENT." | docs in sync |
| Repo scout | "Fetch commits from {REPO} since yesterday and summarize notable changes. If none, output SILENT." | no commits |
| Morning inbox summary | "Fetch unread email via $IMAP_CLI and summarize subjects and senders. If inbox empty, output SILENT." | empty inbox |
| HN briefing | "Fetch top HN story IDs, fetch titles, summarize AI-relevant items. If none, output SILENT." | no relevant items |
| Flight watch | "Run: $FLIGHT_CHECK_CMD. If price dropped below $TARGET output summary. Otherwise output SILENT." | price above threshold |
| Paper digest | "Fetch arxiv.org search for ai+agents and summarize papers from the past 7 days. If none, output SILENT." | no new papers |
