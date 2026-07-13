#!/usr/bin/env bash
# ax: name = coding-project coordinator
# ax: description = one continuous, steerable coordinator for the project in this directory
set -euo pipefail

HARNESS="${AX_HARNESS:-claude}"
DIR="${AX_DIR:-$PWD}"
BEHAVIOR="${AX_COORDINATOR_BEHAVIOR:-$HOME/.config/ax/behaviors/coordinator.md}"
RUN="${AX_RUN:-coord-$(basename "$DIR")}"
GOAL="${1:-Coordinate this project: triage requests into .coordinator/backlog.md, delegate work to workers, verify against evidence, keep docs current. Read .coordinator/backlog.md first; if it is empty, ask me what to take on.}"

mkdir -p "$DIR/.coordinator"

# pi/codex do one burst per turn and then stop, so they need ax's native outer
# loop to stay alive between turns: --self-propel re-invokes the idle coordinator
# until the project is done, a human wait, or the idle cap, and never gives up
# while its workers run. claude sustains its own agent loop and refuses the flag
# (its anti-stall watchdog is the heartbeat wait in behaviors/coordinator.md), so
# pass --self-propel only for the inline harnesses. The propel guards keep their
# defaults on purpose: an open-ended coordinator has no shell-checkable "done" to
# hand --propel-until, and progress detection already counts the run's live
# workers and git state, so no --propel-watch is needed. $PROPEL is left unquoted
# below so it expands to nothing (not an empty arg) for claude.
PROPEL=""
case "$HARNESS" in
  pi|codex) PROPEL="--self-propel" ;;
esac

# --max-workers 2 caps live children so a self-propelled coordinator cannot
# over-spawn. --max-depth 2 lets workers sub-delegate one tier. For a small-model
# coordinator, --max-depth 1 is flatter and safer: no sub-delegation at all.
# --fence best-effort keeps the write fence enforced for claude while letting an
# un-fenceable harness (pi/codex/opencode) launch unfenced-with-warning instead
# of being refused outright.
ax "$HARNESS" "$GOAL" \
  --dir "$DIR" \
  --behavior "$BEHAVIOR" \
  --write './.coordinator/**/*.md' \
  --fence best-effort \
  --no-subagents \
  --max-workers 2 \
  --max-depth 2 \
  --run "$RUN" \
  --name coordinator \
  --label role=coordinator \
  --label recipe=coding-project-coordinator \
  --keep-live \
  $PROPEL \
  --interactive \
  --attach
# Open-ended project: leave --max-cost/--max-tokens off (a tripped fence
# cascade-kills the run). Opt in for a harder worker cap or token stop:
#   --max-workers 4 --max-tokens 5000000
