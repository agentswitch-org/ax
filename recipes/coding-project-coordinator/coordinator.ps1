# ax: name = coding-project coordinator
# ax: description = one continuous, steerable coordinator for the project in this directory
$ErrorActionPreference = 'Stop'

$Harness  = if ($env:AX_HARNESS) { $env:AX_HARNESS } else { 'claude' }
$Dir      = if ($env:AX_DIR) { $env:AX_DIR } else { (Get-Location).Path }
$Behavior = if ($env:AX_COORDINATOR_BEHAVIOR) { $env:AX_COORDINATOR_BEHAVIOR }
            else { Join-Path $HOME '.config/ax/behaviors/coordinator.md' }
$Run      = if ($env:AX_RUN) { $env:AX_RUN } else { 'coord-' + (Split-Path $Dir -Leaf) }
$Goal     = if ($args.Count -gt 0) { $args[0] } else {
  'Coordinate this project: triage requests into .coordinator/backlog.md, delegate work to workers, verify against evidence, keep docs current. Read .coordinator/backlog.md first; if it is empty, ask me what to take on.'
}

New-Item -ItemType Directory -Force -Path (Join-Path $Dir '.coordinator') | Out-Null

# pi/codex do one burst per turn and then stop, so they need ax's native outer
# loop to stay alive between turns: --self-propel re-invokes the idle coordinator
# until the project is done, a human wait, or the idle cap, and never gives up
# while its workers run. claude sustains its own agent loop and refuses the flag
# (its anti-stall watchdog is the heartbeat wait in behaviors/coordinator.md), so
# pass --self-propel only for the inline harnesses. The propel guards keep their
# defaults on purpose: an open-ended coordinator has no shell-checkable "done" to
# hand --propel-until, and progress detection already counts the run's live
# workers and git state, so no --propel-watch is needed. $Propel stays an array
# (direct assignment, not an `if` expression, which would unwrap the single
# element to a string and make @Propel splat it character by character) so
# splatting @Propel adds one arg for pi/codex and zero for claude (never an empty
# arg, unlike a bare $null).
$Propel = @()
if ($Harness -eq 'pi' -or $Harness -eq 'codex') { $Propel = @('--self-propel') }

# --max-workers 2 caps live children so a self-propelled coordinator cannot
# over-spawn. --max-depth 2 lets workers sub-delegate one tier. For a small-model
# coordinator, --max-depth 1 is flatter and safer: no sub-delegation at all.
# --fence best-effort keeps the write fence enforced for claude while letting an
# un-fenceable harness (pi/codex/opencode) launch unfenced-with-warning instead
# of being refused outright.
ax $Harness $Goal `
  --dir $Dir `
  --behavior $Behavior `
  --write './.coordinator/**/*.md' `
  --fence best-effort `
  --no-subagents `
  --max-workers 2 `
  --max-depth 2 `
  --run $Run `
  --name coordinator `
  --label role=coordinator `
  --label recipe=coding-project-coordinator `
  --keep-live `
  @Propel `
  --interactive `
  --attach
# Open-ended project: leave --max-cost/--max-tokens off (a tripped fence
# cascade-kills the run). Opt in for a harder worker cap or token stop:
#   --max-workers 4 --max-tokens 5000000
