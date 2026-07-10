# ax recipes

Practical composition patterns for ax. Two kinds of artifact live here, and the
split is the whole point.

A **recipe** is a shell workflow, shipped as a bundle: an entry script plus the
assets it needs (adapters, rubrics, an accept check, a `recipe.md`). It has
deterministic control flow in bash (loops, gates, exit codes) and hands judgment
to the sessions it launches. You copy the directory, edit the task text, and run
the entry script from a shell or cron.

A **behavior** is a markdown prompt bound to one session with `--behavior`. It is
never executed. It defines the durable role a session plays (coordinator,
producer, reviewer, triage worker), its operating loop, and its output contract.
A recipe launches sessions and may bind behaviors to them. A behavior does not
run a recipe by itself.

The boundary: role, judgment, and discipline live in a behavior. Branching,
loops, gates, exits, file generation, and mechanical checks live in a recipe.

## Two ways to run a workflow

**Run a recipe directly.** Copy the recipe directory, edit the task text, and run
its entry script. This suits a scheduled job or a known one-shot.

**Hand the coordinator a goal.** The coordinator is a behavior, the master one: a
session launched wearing `behaviors/coordinator.md` can run every recipe here on
request, because a recipe is shell and the coordinator runs shell. Describe a
problem in plain language and it decides the shape of the work, launches the
right workers and binds the right behaviors, supervises them, and checks the
result against your criteria. It is the operator that runs the recipes, not one
of them.

## Behaviors

Standalone behaviors (not part of any one recipe's bundle) live at the repo root
in `behaviors/`.

| File | Purpose |
|---|---|
| [../behaviors/coordinator.md](../behaviors/coordinator.md) | the master behavior: one continuous, self-propelled coordinator per project. Runs an event loop forever, triages requests into `.coordinator/backlog.md`, delegates every change to workers, verifies against evidence; the human detaches and reattaches to steer |

Copy it to a stable path and pass that path to `--behavior`:

```bash
cp behaviors/coordinator.md ~/.config/ax/behaviors/coordinator.md
ax claude "your goal ..." --behavior ~/.config/ax/behaviors/coordinator.md --write './.coordinator/**/*.md'
```

A recipe's own behaviors stay nested under the recipe in its `behaviors/`
directory, because the recipe binds them and its scripts reference them
relatively.

## Recipes

| Recipe | What it does | Ships |
|---|---|---|
| [coordinator](coordinator/) | Launch the coordinator behavior under a markdown write fence so it must delegate all real work | `recipe.md` |
| [coding-project-coordinator](coding-project-coordinator/) | Bootstrap one continuous, steerable coordinator onto the project in the current directory | `coordinator.sh`, `coordinator.ps1` |
| [cost-routing](cost-routing/) | Run a task on haiku first, escalate to sonnet then opus on failure | `escalate.sh`, `check.sh` |
| [blackboard](blackboard/) | Two workers coordinate through a shared JSON file on disk | `run.sh` |
| [vault-fanout](vault-fanout/) | Fan N workers in parallel over a corpus, join on one verify pass | `fanout.sh`, `check-vault.sh`, `split-mbox.sh` |
| [behavior-audit](behavior-audit/) | Read session history, derive curated behavior-file edits | `audit.sh` |
| [email-triage](email-triage/) | Classify and act on a maildir, notify only on important items | `triage-wrapper.sh`, `behaviors/email-triage.md` |
| [pod-dryrun](pod-dryrun/) | Fan-out design pipeline with a human-gated publish step | `behaviors/pod.md`, `scripts/`, `starter/niches.json` |
| [prescript-formatter](prescript-formatter/) | Deterministic check feeds an LLM formatter with a sentinel gate | `run-health-check.sh`, `check-services.sh`, `behaviors/lm-formatter.md` |
| [scheduled-chain](scheduled-chain/) | Cron trigger, ax chain, sentinel gate, notify | `ax-scheduled-chain.sh`, `behaviors/worker.md`, `notify-stub.sh` |
| [taste-gate](taste-gate/) | Produce, score against a rubric, iterate to a threshold or report the cap | `taste-loop.sh`, `behaviors/`, `rubrics/` |

Every recipe directory carries a `recipe.md` with its folder layout, an ax
composition diagram, how-to-run instructions, and fences. Copy the directory,
adapt the task string to your domain, and run the entry script.

## Recipe header

An entry script can carry an optional header so the picker shows a friendly name
and one-line description instead of the bare filename. ax reads leading comment
lines of the form:

```sh
# ax: name = Coding Project Coordinator
# ax: description = Bootstrap one steerable coordinator onto this project
```

Rules: both keys are optional and cosmetic; `name` overrides the filename-derived
label, `description` becomes the picker subtitle and the launched session's task.
Only `# ax: name = ...` and `# ax: description = ...` are recognized (case
insensitive on the key, whitespace around `=` trimmed); any other `# ax: key = value`
line is ignored. The header must sit in the leading comment block: blank lines and
other comment lines (including the shebang) are skipped, but scanning stops at the
first real command, so keep the header above the script body.

## Test apparatus

The `tests/recipes/` tree at the repo root holds fixtures, corpus generators, and
recorded proof runs. They re-verify the recipes after a CLI change, but they are
not part of any copyable bundle. A recipe stays runnable without them. You reach
for them only to reproduce a proof or generate demo input.

## No configuration required

No ax setup beyond a working `ax` install is needed. Recipes default to
subscription OAuth (zero per-token cost). `--api` is opt-in only where a recipe
documents it. On subscription auth `--max-cost` is inert, so `--max-tokens` is
the fence that binds.
