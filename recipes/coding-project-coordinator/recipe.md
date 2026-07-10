# Recipe: Coding-project coordinator (bootstrap)

One command that bootstraps the continuous coding-project coordinator onto the
project in the current directory. It launches a single session wearing
`behaviors/coordinator.md` under a markdown write fence (`--write
'./.coordinator/**/*.md'`, `--no-subagents`), named `coordinator`, held live
with `--keep-live`, in a run named after the project directory. The
coordinator then runs its event loop indefinitely: it triages requests into
`.coordinator/backlog.md`, delegates every change to ax workers, verifies
results against evidence, and stays until you end the project. You detach and
reattach to steer it.

This is the bootstrap for the behavior; the full launch anatomy (what each
flag does and why) is documented in [../coordinator/recipe.md](../coordinator/recipe.md),
and the operating contract lives in
[../../behaviors/coordinator.md](../../behaviors/coordinator.md).

## Ships

| File | Purpose |
|---|---|
| `coordinator.sh` | bash entry script (Linux/macOS) |
| `coordinator.ps1` | pwsh entry script (Windows) |

Both scripts are the same launch; copy whichever fits the host. They assume
the behavior file is installed at `~/.config/ax/behaviors/coordinator.md`
(override with `AX_COORDINATOR_BEHAVIOR`). `--behavior` always points at the
full file, however large: ax spills an oversized launch command to a temp
script instead of handing it to the terminal multiplexer inline, so a
behavior file of tens of kilobytes launches intact.

## How to run

From the project root, one-shot:

```bash
cp behaviors/coordinator.md ~/.config/ax/behaviors/coordinator.md
recipes/coding-project-coordinator/coordinator.sh
# or with an initial goal:
recipes/coding-project-coordinator/coordinator.sh "Ship the v2 importer. Done when: go test ./... passes and the README covers the new flow."
```

Or from the picker: `recipes_dir` lists direct files only, so copy the
matching script into it (`cp recipes/coding-project-coordinator/coordinator.sh
~/.config/ax/recipes/`) and launch it from the compose flow. ax execs recipe
files tracked, exporting `AX_RUN`, `AX_HARNESS`, and `AX_DIR` into the
script's environment; the `# ax: name = ...` / `# ax: description = ...`
header lines are the picker's display metadata.

Environment knobs, all optional: `AX_HARNESS` (default `claude`), `AX_DIR`
(default `$PWD`), `AX_RUN` (default `coord-<dirname>`),
`AX_COORDINATOR_BEHAVIOR` (default `~/.config/ax/behaviors/coordinator.md`).

## Fences

The launch is open-ended on cost/tokens by design, because those tripped fences
cascade-kill the whole run, which is wrong for a coordinator that lives as long
as the project. It still sets `--max-workers 2` so a self-propelled coordinator
cannot over-spawn live children. Raise the worker cap or opt in to a token stop
by editing the launch: `--max-workers 4 --max-tokens 5000000`. `--max-depth 2`
lets workers sub-delegate one tier; drop to `--max-depth 1` for a flatter,
safer topology under a small-model coordinator.

## Small/local-model coordinator

For a weak local model (a 12B, say), point the launch at the trimmed
behavior instead of the full one: `AX_COORDINATOR_BEHAVIOR=behaviors/coordinator-small.md`
(the recipe already reads this env var). A 12B follows a short literal spec
far better than the full behavior, which is long and eats its context. Also
set `AX_DIR` to the project folder itself, not the home directory, so the
coordinator's scope stays narrow, and give the server a large enough context
window that accumulated tool output does not overflow it. These three
settings, proven together in a live eval, are what let a Gemma-4-12B
coordinator build a project end to end and report `PROJECT-COMPLETE`.
