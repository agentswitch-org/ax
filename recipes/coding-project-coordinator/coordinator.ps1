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
  --interactive `
  --attach
# Open-ended project: leave --max-cost/--max-tokens off (a tripped fence
# cascade-kills the run). Opt in for a harder worker cap or token stop:
#   --max-workers 4 --max-tokens 5000000
