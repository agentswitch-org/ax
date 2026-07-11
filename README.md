# agentswitch (ax)

ax is a daemonless control substrate for CLI coding agents. It turns local and
remote Claude Code, Codex, opencode, and pi sessions into addressable workers
that can be launched, detached, resumed, read, waited on, tagged, killed, and
sent input from the command line.

That makes ax both a session switcher and a coordination layer. Humans get one
searchable TUI for jumping between live sessions; agents and scripts get a small
verb set for self-propelled runs that need to spawn workers, watch transcripts,
exchange messages, enforce fences, and converge on a result without a daemon.

Full manual, tutorials, and recipes: [agentswitch.org](https://agentswitch.org)

## Install

Release builds are published for macOS, Linux, and Windows on amd64 and arm64.

### macOS

Homebrew is the recommended macOS install:

```sh
brew install --cask agentswitch-org/ax/ax
ax version
```

You can also use the standalone installer, which installs `ax` to
`~/.local/bin` unless `AX_INSTALL_DIR` is set:

```sh
curl -fsSL https://agentswitch.org/install.sh | sh
```

### Linux

The standalone installer downloads the matching tarball from the latest GitHub
release and installs `ax` to `~/.local/bin` unless `AX_INSTALL_DIR` is set:

```sh
curl -fsSL https://agentswitch.org/install.sh | sh
ax version
```

Debian, Ubuntu, Fedora, RHEL, Rocky, and AlmaLinux users can also install the
`.deb` or `.rpm` attached to the latest GitHub release:

```sh
sudo apt install ./ax_<version>_linux_<arch>.deb
sudo dnf install ./ax_<version>_linux_<arch>.rpm
```

Use `amd64` or `arm64` for `<arch>`.

### Windows

Run this in PowerShell:

```powershell
irm https://agentswitch.org/install.ps1 | iex
ax version
```

The installer downloads the matching `.zip` from the latest GitHub release,
installs `ax.exe` to `%LOCALAPPDATA%\ax\bin`, and adds that directory to your
user `Path`.

Manual install is also fine: download `ax_<version>_windows_<arch>.zip` from
the latest GitHub release, extract `ax.exe`, and place it on `Path`.

### Updating

Re-run the same installer command to update. On Windows, the installer places
each release under `%LOCALAPPDATA%\ax\versions\<version>` and moves that
directory to the front of the user `Path`, so updates work even when an older
`ax.exe` is still running. Homebrew users should update with:

```sh
brew upgrade --cask agentswitch-org/ax/ax
```

### Release candidates

Release candidates use tags such as `v0.1.1-rc.1`. They publish GitHub
prerelease assets and a separate Homebrew cask:

```sh
brew install --cask agentswitch-org/ax/ax-rc
curl -fsSL https://agentswitch.org/install.sh | AX_RELEASE_TAG=v0.1.1-rc.1 sh
```

On Windows:

```powershell
$env:AX_RELEASE_TAG = 'v0.1.1-rc.1'; irm https://agentswitch.org/install.ps1 | iex
```

### Developer installs

With Go 1.26.4 or newer:

```sh
go install github.com/agentswitch-org/ax/cmd/ax@latest
```

From a source checkout:

```sh
git clone https://github.com/agentswitch-org/ax.git
cd ax
make install
```

`make install` places `ax` in `~/.local/bin`. On stock macOS this directory is
not on `PATH` by default.

## Quick start

Start a project coordinator from the project root.

macOS/Linux:

```sh
curl -fsSL https://agentswitch.org/coordinator.sh | sh
```

Windows PowerShell:

```powershell
irm https://agentswitch.org/coordinator.ps1 | iex
```

The helper refreshes the coordinator behavior and recipe under `~/.config/ax`,
then launches the recipe in the current directory.

## Common commands

```
ax                       open the picker
ax new [harness [flags]] start a new session
ax claude "fix the flaky test"    run a task, tracked in a background window
ax codex "add a CHANGELOG entry"  same, with codex
ax "fix the flaky test"           same, when default_harness is set in config
```

For multi-line prompts, pass `--task-file <path>` instead of shell-quoting a
long string. `-` reads from stdin:

```sh
ax claude --task-file task.md          # task text comes from a file
cat task.md | ax claude --task-file -  # or piped through stdin
ax continue <id> --task-file task.md   # same for ax continue
```

To wait for results, launch in the background, then run `ax wait` and
`ax result` after the run concludes:

```sh
id=$(ax claude "do the migration" --json | jq -r .id)
ax wait "$id"     # blocks until done; exit code reflects the outcome
ax result "$id"   # prints its captured final report
```

Avoid long blocking `ax wait --timeout 30m` in the foreground when running a
fleet because it stalls supervision of all other workers in the run. For live
supervision of only the workers active now, without replaying an old run's
history, run:

```sh
ax read --run "$AX_RUN" --follow --active --from-now --exclude "$AX_SESSION_ID"
```

Use the existing `ax read --run "$AX_RUN" --follow --limit 1 --exclude "$AX_SESSION_ID"`
loop when automation wants exactly one worker event and then returns to decide
whether to `ax wait` / `ax result`. `--exclude-self` is also available when
`AX_SESSION_ID` is reliably present.

To supervise workers across every configured host, add `--hosts` (alias
`--federated`), which requires `--run`:

```sh
ax read --run "$AX_RUN" --follow --active --from-now --hosts --exclude-self --timeout 10m
```

`--hosts` fans the run read out to the local machine and each configured host,
preserving the default local-only behavior when omitted. Each JSON event carries
`host` plus a routeable `id` (`host/id` for remotes). `--from-now` seeds each
host's active workers at their current cursor before following, so stale
transcript history is not replayed. With no new event before `--timeout`, the
command exits quietly with no output. Omit `--hosts` for the older local-only loop.

Set `default_harness = "claude"` in `~/.config/ax/config.toml` once, and `ax "PROMPT"` dispatches to claude everywhere instead of `ax claude "PROMPT"`. The [Configuration](#configuration) section has the full setup and the precedence rules.

If you run tmux, you can bind the picker to a key. The prefix is whatever you use. This is optional; ax does not require tmux:

```
bind-key a display-popup -E -B -s 'bg=terminal' -w 100% -h 100% "ax pick"
bind-key A display-popup -E -B -s 'bg=terminal' -w 100% -h 100% "ax new"
```

`<prefix> a` opens the picker. `<prefix> A` starts a new session. Use `-w 85% -h 80%` for a floating box, or `run-shell` with the bundled `tmux/ax.tmux`.

## Description

ax ships as a single Go binary. It reads the transcript files your harnesses
already write, keeps sessions addressable after detach, and resumes a session by
running that harness's own resume command. It does not replace the harness and
has no LLM of its own.

There is a small internal session manager built in. It keeps a harness alive
after `ctrl-a` then `d`, so you can launch, detach, monitor, and reattach
without tmux or zellij. If you already use tmux or zellij, ax can drive those
backends instead.

The shell verbs let one session drive others: launch workers, read turns, send
messages, wait for completion, run an accept check, and stop a run. Fences on
cost, fan-out, depth, and time apply across the run.

ax ships no installed recipes, behaviors, coordinator prompt, or presets. The `recipes/` and `behaviors/` directories in this repo are source files to copy from. Configure `behaviors_dir` and `recipes_dir` to point at your own files, then use the compose chooser (`c`, `C`, or `ctrl-n`) to launch a plain session, a behavior-backed prompt, or a recipe script.

## Supported platforms

macOS, Linux, and Windows (amd64 and arm64).

On Windows ax runs natively. The built-in `process` backend puts each harness under a [ConPTY](https://learn.microsoft.com/en-us/windows/console/pseudoconsoles) confined to a kill-on-close job object. It drives steering input (`ax send`, interrupts) over a per-session named pipe, so the full launch, detach, monitor, reattach loop works with no external dependency. The process backend is the native default on Windows. tmux popup/window-management features are Unix mux features; leave `mux` unset unless you have verified another backend in your own Windows terminal stack. WSL remains an option when you want the POSIX toolchain.

Windows hosts work in federation too. A host such as `win01` can be added with `transport = "ssh -t win01"`; if its sshd default shell is PowerShell, set `shell = "pwsh"` in that `[[host]]` entry so remote arguments are quoted correctly. Mark the host `headless = true` when remote interactive launch/attach has not been verified for that box. Remote ids show up as `win01/<id>`, and `ax config status` reports each host's OS, shell, ax version, and wire compatibility.

## Runtime requirements

- No mux is the default. ax's own session holder keeps each harness alive when you detach, so `ctrl-a` then `d` returns you to the shell or picker and the session keeps running. `ax attach <id>` drops you back in. This is the native `hold_backend`, ax's holder built into `ax run`, with no external binary. `dtach` is optional and only used for `hold_backend = "dtach"`. `hold_backend = "none"` turns holding off.
- `tmux` and `zellij` are optional: if you run one, ax can open and manage a window or tab for each session. Pick a backend with the `mux` config setting. The no-mux backends are `process` and `none`.
- `ripgrep` (optional): speeds up transcript content search.
- `zoxide` (optional): supplies directory candidates when starting a new session.

For a remote host, `ax` must be installed there too. Install `dtach` there only if that host explicitly uses the dtach hold backend.

## Keys

The picker opens in normal mode. Press `?` for the full key map.

- `j`/`k` move, `g`/`G` top/bottom, `d`/`u` half-page
- `J`/`K` scroll the preview, `n`/`N` next/previous content-search match
- `i` filter rows by the highlighted column, `/` search transcript text
- `t` cycle scope (all/live/working/active-run), `A` cycle archive visibility (unarchived/all/archived), `m` filter by machine, `f` filter by run, `b` cycle group-by pivot
- `T` toggle the run tree view, `H`/`L` pick a sort column, `s` sort (again to flip)
- `Enter` open or resume (jumps to its window if already open), `e`/`E` resume without/with the harness's flags
- `c`/`C`/`ctrl-n` open the compose chooser (harness, then a mode of plain / behavior+prompt / recipe, then a directory), `x` kill
- `r` reply to a blocked `ax ask` question, `Tab` multi-select, `v` visual select
- `l` tag the selection, `D` archive selected non-live rows or unarchive archived rows, `M` move windows into a named tmux session
- `q` quit

Inside an attached session, `ctrl-a` then `d` detaches (the screen chord): you return to the shell and the session keeps running (`ax attach <id>` to come back). `ctrl-a` then `a` detaches the same way but reopens the ax picker in the same terminal, so you can hop between sessions without a multiplexer. Only the prefix is intercepted: `ctrl-a` `ctrl-a` sends one literal `ctrl-a` to the harness, and `ctrl-a` followed by any other key passes both through. Rebind the prefix with `detach_prefix` (any `ctrl-<letter>`), the detach letter with `detach_key`, and the menu letter with `menu_key`; `ctrl-\` always works as a detach fallback.

Any key can be rebound under `[keys]` in the config. The help screen and hint row always show the keys you set.

Press `` ` `` (the leader, rebindable as `bind`), then a key, to run your own shell command. `[[bind]]` in the config maps a key to a command template with placeholders for the selected session (`{id}` `{run}` `{dir}` `{transcript}`) and a per-binding fixed `{file}`. The leader gives user bindings their own namespace, so they do not collide with built-in keys. See `config.example.toml`.

## Configuration

Claude Code, Codex, pi, and opencode are built in and work with no config file. To add or change a harness, copy `config.example.toml` to `~/.config/ax/config.toml`. Each `[[harness]]` says where its transcripts live and how to resume a session.

Set `default_harness` to drop the harness name from the command line:

```toml
default_harness = "claude"
```

With this set, `ax "fix the bug"` launches claude with that prompt. Known verbs (`list`, `kill`, `read`, etc.) and harness names always take precedence over the bare-prompt path, so `ax list` still lists sessions and `ax claude "x"` still launches claude explicitly. If a prompt collides with a reserved word, use `ax <harness> "<prompt>"` to force it.

## Remote hosts

ax merges sessions from other machines into one list over SSH or any transport:

```toml
[[host]]
name      = "laptop"
transport = "ssh -t user@laptop"
```

No daemon and no open port. ax runs the transport command, which carries its own auth. Pick a remote session and ax attaches over the transport in a local window. `ax kill`, `ax read`, and the rest route the same way. `m` filters the list by machine and shows each host's status: online, offline, `no ax`, or `old ax`.

Plain `ax list` and a remote machine's own `ax` command read only that machine's local index. Use `ax list --federated --all` to include the hosts configured on the machine where you run the command. If history looks empty in the picker, widen the scope with `t` and the archive view with `A`; those view choices are stored in `$XDG_STATE_HOME/ax/ui.json`.

If a host keeps its harness behind a tool manager (mise, nvm, asdf), set `shell = "zsh -lic"` in that host's own ax config so `ax` resolves the harness the same way your terminal does.

## Running agents safely

By default ax launches each agent in its harness's autonomous or permission-bypass mode so agents run without blocking for input. For Claude Code this means `--dangerously-skip-permissions`; for Codex this means full access mode. Agents run unattended in this posture, so run them in an environment you trust (a VM, a container, or a dedicated machine where the agent has limited reach).

To remove the bypass flag for a harness, edit its `args` in `~/.config/ax/config.toml`:

```toml
[[harness]]
name = "claude"
args = []   # omit --dangerously-skip-permissions to restore the normal permission prompts
```

Folder trust: when ax launches a session in a directory, it pre-accepts the Claude Code folder-trust dialog for that directory by writing `hasTrustDialogAccepted` into `~/.claude.json`. This clears the safety prompt Claude Code would otherwise show. Be aware of this if you launch sessions in directories you have not reviewed.

## Known limitations

- Remote compose: picking a remote target in the compose chooser aborts with an explicit unsupported message rather than falling back to a plain remote launch. Choose local for composed behavior/recipe launches. For remote work, run `ax <harness> "task" --host H`, `ax new` on that host, or an explicit transport command.
- Write fence coverage: the `--write`/`--no-write` fence is hook-enforced for Claude Code only. Fencing a harness ax cannot enforce (codex, pi, opencode) refuses the launch. `--fence best-effort` opts into launching it unfenced with a warning. The cost, token, worker, depth, and time fences apply to every harness.
- Self-propel maturity: `--self-propel` (pi and codex) is new in 0.1.0. It targets one-burst local-model workflows and may have rough edges elsewhere. Cloud harnesses sustain their own loop and do not need it.

## Data and privacy

ax sends nothing anywhere. There is no telemetry and no account. Its only network use is the explicit `ax models update` command, which fetches the public model-price catalog from models.dev, plus the SSH or other transports you configure for remote hosts. Set `offline = true` in `~/.config/ax/config.toml` (or `AX_OFFLINE=1` in the environment) to block even the model update call; ax falls back to bundled or cached model data.

ax stores its state under `$XDG_STATE_HOME/ax` (default `~/.local/state/ax`): session metadata, run records, heartbeat files, and a plain-text search cache of your transcripts. Config lives at `~/.config/ax/config.toml`. ax reads your harness's transcript stores (e.g. `~/.claude/projects/`) but never modifies them.

Running `ax hook install claude` merges lifecycle hooks into `~/.claude/settings.json`; ax announces the write and is idempotent.

To uninstall, remove the binary, the state dir, and the config, then strip the ax hook entries from `~/.claude/settings.json`:

```
rm ~/.local/bin/ax
rm -rf ~/.local/state/ax ~/.config/ax
# edit ~/.claude/settings.json to remove the ax hookstate lines
```

ax is licensed under the MIT License, see the LICENSE file.

## Everything else

The full command reference, control plane (launch verbs, fences, the coordinator, `ax metrics`), running agents safely, configuration details, and model data are at [agentswitch.org](https://agentswitch.org).
