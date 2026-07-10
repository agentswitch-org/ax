# Recipe: Coordinator under a write fence

Launch one session that drives a whole job by delegating to workers. It wears
`behaviors/coordinator.md`, keeps its own state as markdown, and cannot touch
anything else. The fence is what forces the delegation: the session can write its
own `.coordinator/*.md` files and nothing more, so every real change goes through
a worker it launches with `ax`.

This is the first-class entry point to every other recipe. Hand the coordinator a
goal in plain language and it decides the shape of the work, launches the right
workers, supervises them, and checks the result against its own criteria.

---

## The launch command

```bash
ax claude "<task>" --behavior behaviors/coordinator.md \
  --write './.coordinator/**/*.md' --no-subagents --max-depth 2 --label role=coordinator [--interactive]
```

Run it from the repo you want the work done in. The task string states the goal
and the acceptance criteria (`Done when: ...`). The coordinator prints its session
id; watch it in the picker or with `ax read <id> --follow`.

---

## Flags

| Flag | Why |
|---|---|
| `--behavior behaviors/coordinator.md` | the coordinator system prompt: how it delegates, supervises, and verifies. Without it this is just a plain agent |
| `--write './.coordinator/**/*.md'` | the markdown state carve-out, and the fence itself. The session may write files matching this glob (its `state.md`, `backlog.md`, `plan.md`) and nothing else. Every other file write and all mutating shell is denied, which is what forces it to delegate real work to workers |
| `--no-subagents` | required. Turns the Task/Agent tools off so every unit of work goes through `ax claude`, where it is tracked, attachable, killable, and fenced by its own launch flags. That is the coordinator model: delegate through `ax`, not through untracked in-process subagents. It is also defense in depth, since a subagent on a harness build where the fence does not reach it would get an ungated shell. It is not a write sandbox, the write boundary is `--write` / `--no-write` |
| `--max-depth 2` | the recursion budget. The coordinator is depth 0 and its workers are depth 1, so the default of 1 already lets it launch a flat fleet of workers. `2` gives one more tier, so a worker can itself launch workers (a sub-coordinator). Raise it for deeper divide-and-conquer |
| `--label role=coordinator` | metadata for `ax list`, not read by core. It marks the row so you can filter the run and tell the coordinator apart from its workers |
| `--interactive` | explicit "run it watched so I can attach in the picker." Watched is already the default on a subscription, so this documents intent more than it changes behavior. For a headless job, drop it and add `--wait`. Either way its workers run on subscription OAuth at zero per-token cost |

---

## The fence

`--write` engages the capability fence with a positive write scope. The rule is
simple: a file write lands only when its path matches one of the `--write` globs,
and all mutating shell is denied. So the coordinator writes its own markdown state
through the Write/Edit tools, and everything else, code, config, data, git
commits, is refused at the harness. It cannot cheat past the fence by "deciding"
to edit a file. That refusal is the whole point: it turns every write the
coordinator is tempted to do into an instruction for a worker.

The fence is per session, never inherited. A worker the coordinator launches
(`ax claude "..."` with no `--write`) is fully writable. The scope lives in the
coordinator's `$AX_WRITE` env, and it is not passed down to a child.

`--no-write` is the zero-scope variant: no file writes at all. Use it for a
coordinator that keeps no state file and delegates every write, including its own
notes.

---

## Why no subagents

The canonical launch passes `--no-subagents`, which removes the Task/Agent tools.
This is required, not optional. It is first a delegation and tracking control. With
Task off, the coordinator delegates every unit of work through `ax claude`. An `ax`
worker is a real session: it is writable, joins the run, records the coordinator as
its parent, and shows up in `ax list`. Its fence, or lack of one, comes from its own
launch flags. That worker is tracked, attachable, and killable, which is the whole
coordinator model: delegate through `ax`, not through untracked in-process
subagents.

It is also defense in depth. Whether the fence reaches a Task subagent depends on
the Claude Code harness version. On the current claude it does reach them, because a
subagent's tool calls hit the same PreToolUse hook and share the process env, but
that has not held on every build. On a build where the subagent hook does not fire,
the subagent gets an ungated shell, because `Bash` has no deny-list backstop the way
the write tools do. A coordinator should not rely on a third-party harness behavior
it does not control, so it does not spawn subagents it cannot govern.

Do not read `--no-subagents` as a write sandbox. It is not one. The write boundary
is `--write` / `--no-write`, and both the write fence and the shell classifier are
leaky by construction: a fenced session can still run `ax claude` to spawn a
writable worker, by design. OS-level isolation is the only un-foolable layer. The
point of `--no-subagents` is to keep all work on tracked `ax` sessions.

`--no-subagents` itself stays a general capability (turn off Task/Agent for any
session). The requirement is specific to the coordinator recipe.

---

## When to use this recipe

- The job is big enough to split across workers, or needs isolation or a fresh
  perspective per piece.
- You want one session to own the plan, launch the pieces, judge the results, and
  conclude when the criteria pass, without you sequencing each step.
- You want a durable, inspectable record: the coordinator's state and backlog sit
  in `.coordinator/*.md`, and every worker is a session in the run.

For a single self-contained change, skip the coordinator and launch a plain
worker. Delegation only pays when it buys parallelism, isolation, or a second
perspective.

---

## How it delegates

1. It reads the task, writes the goal and acceptance criteria to
   `.coordinator/state.md`, and picks the shape of the work.
2. It launches workers with `ax claude "SUBTASK" --label role=worker`, each in its
   own workspace when they might touch the same files.
3. It supervises with `ax read --run "$AX_RUN" --follow --limit 1 --exclude "$AX_SESSION_ID"`
   as a heartbeat, reads each worker's output, and accepts, corrects, or relaunches.
4. It verifies "done" with evidence: `ax check` for the run's accept check, and
   the Read/Grep tools for artifacts. It never trusts a worker's claim.
5. When the criteria pass, it tags its outcome and reports.

The full operating contract is in `behaviors/coordinator.md`.

---

## A one-shot `ax coordinator` wrapper

If you launch coordinators often, wrap `ax` in your shell so `ax coordinator
"TASK"` expands to the launch above. This is your own setup, not shipped by ax.
