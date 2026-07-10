# Changelog

All notable changes to ax are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/).

## 0.1.0 - 2026-07-10

First public release.

ax is a daemonless session switchboard and control plane for command-line
coding agents. It reads the transcripts each agent harness already writes
(Claude Code, Codex, opencode, pi), lists every session in a unified terminal
picker, and resumes any session by running that harness's own resume command.
ax has no LLM of its own, no daemon, no accounts, and no telemetry.

### Added

- **Unified picker** across Claude Code, Codex, opencode, and pi: one
  searchable list with filtering, transcript content search, session grouping
  by project or run, sortable columns, a transcript preview, and a run-tree
  view for multi-agent work.
- **Session holding without a mux**: ax's own holder keeps harnesses alive
  when you detach (`ctrl-a d`), with `ax attach` to return. tmux and zellij
  backends supercharge this when present; none is required.
- **Shell verbs** to launch and drive sessions: `ax claude "task"`, `ax new`,
  `ax continue`, `ax restart`, `ax read`, `ax send`, `ax wait`, `ax result`,
  `ax tag`, `ax ask`, `ax reply`, `ax kill`, `ax search`, `ax runs`,
  `ax metrics`, `ax check`. One session can launch and steer others through
  these verbs.
- **Hard fences** on cost, tokens, fan-out, recursion depth, and time. Limits
  set at launch are enforced mechanically, cascade-killing a run when one
  trips.
- **Write fence**: `--write GLOB` confines a session's file writes to matching
  paths and denies mutating shell; `--no-write` allows no writes at all;
  `--no-subagents` denies in-process subagents. Fencing is fail-closed: a
  harness ax cannot fence refuses a fenced launch unless `--fence best-effort`
  opts into launching unfenced with a warning.
- **Compose chooser** (`c`, `C`, or `ctrl-n` in the picker): walk a launch
  through a harness, a mode of plain / behavior+prompt / recipe, and a
  directory. Behaviors are your instruction files (`--behavior PATH`,
  path-only, or `--behavior-text` inline); recipes are your flat scripts, run
  as tracked ax runs. ax ships no recipes, behaviors, prompts, or presets; it
  reads content you own via the `behaviors_dir` and `recipes_dir` config keys.
- **Remote host federation**: sessions from other machines fold into the same
  picker over SSH or any transport you configure, with no daemon and no open
  port. `ax config sync` pushes a portable settings profile to your hosts.
- **Self-propel** (experimental, pi and codex): `--self-propel` re-invokes a
  session that ends its turn with the task unfinished, so a one-burst local
  model keeps grinding until a done sentinel, an accept check, a human wait,
  or an idle cap stops it.
- **Windows native support**: the process backend runs each harness under a
  ConPTY confined to a kill-on-close job object and drives steering input over
  a per-session named pipe.
- Lifecycle hooks (`ax hook install`), notify integrations (bell, tmux, or any
  shell command), extensions (`ax-foo` on PATH becomes `ax foo`), retention
  and reaping controls, cost/token metrics with a Prometheus textfile export,
  and an offline mode.

### Known limitations

- Remote compose aborts with an explicit unsupported message instead of falling
  back to a plain remote launch. Choose local for composed behavior or recipe
  launches. For remote work, run `ax <harness> "task" --host H`, `ax new` on
  that host, or an explicit transport command.
- The write fence is hook-enforced for Claude Code only; codex, pi, and
  opencode refuse fenced launches unless `--fence best-effort`.
- `--self-propel` is new; expect rough edges outside the pi/codex local-model
  workflows it was built for.
