// Package config defines the harness registry: where each LLM CLI stores its
// sessions, how to pull a session id out of a transcript path, and how to
// resume or launch it. Defaults cover claude and pi; ~/.config/ax/config.toml
// overrides or extends them.
package config

import (
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/agentswitch-org/ax/internal/notify"
)

// Harness is one LLM CLI ax knows how to read and drive.
type Harness struct {
	// Name is the short label shown in the picker (e.g. "claude", "pi").
	Name string `toml:"name"`
	// Glob matches every session transcript on disk. A leading ~ is expanded.
	Glob string `toml:"glob"`
	// IDRe is a regex over the full transcript path with a named capture
	// group `id` that yields the session id used for resume.
	IDRe string `toml:"id_regex"`
	// Format selects the built-in parser ("claude", "pi", "codex", "opencode").
	Format string `toml:"format"`
	// DB is the SQLite database path for db-backed harnesses (opencode). A
	// leading ~ is expanded. Ignored by file-glob harnesses.
	DB string `toml:"db"`
	// Resume is the command template run to continue a session. Placeholders:
	// {id} {dir} {model} {args}. A " --model {model}" fragment is dropped when
	// the model is unknown; the {args} slot is filled from Args (or a per-launch
	// override) and dropped when empty.
	Resume string `toml:"resume"`
	// ResumeInput is the command template `ax continue` runs to resume a session
	// with a NEW task delivered as input, the resume-with-input primitive between
	// `ax send` (needs a live tmux window) and a cold launch. Same placeholders as
	// Resume plus {task} (the new prompt). Empty means the harness has no cleanly
	// documented resume-with-input form, so `ax continue` degrades gracefully with
	// a message rather than guessing. ResumeInputHeadless is the same for a
	// `--wait`/`--headless` job (empty falls back to no headless continue).
	ResumeInput         string `toml:"resume_input"`
	ResumeInputHeadless string `toml:"resume_input_headless"`
	// Launch is the command template to start a fresh session in {dir}. The
	// {args} slot works the same as in Resume. A {newid} slot, when present, is
	// filled with a freshly minted session id passed to the harness (e.g.
	// "claude --session-id {newid}") so ax can tag and track the window from the
	// start instead of losing it until the harness writes its first transcript.
	Launch string `toml:"launch"`
	// Args are default extra flags injected into the {args} slot of Launch and
	// Resume (e.g. "--dangerously-skip-permissions"). Empty by default; the picker
	// applies them with `C` (new) and `E` (resume), while `c`, Enter, and `e`
	// launch without them. When a template has no {args} placeholder the flags are
	// appended instead.
	Args string `toml:"args"`
	// LaunchHeadless is the command template for a job-mode (`--wait`/`--unattended`)
	// control-layer launch: runs the task to completion and exits, no tmux. Same
	// placeholders as Launch. Empty means the harness has no headless form, so a
	// job launch falls back to Launch under tmux.
	LaunchHeadless string `toml:"launch_headless"`
	// WaitingRe is an optional regex matched against the pane tail to tell a worker
	// that went idle apart because it is blocked on a sub-prompt (permission y/n,
	// an OAuth login) rather than done. Feeds the `waiting` follow event.
	WaitingRe string `toml:"waiting_re"`
	// SkipPermissions is the harness's own flag (or flags) for making a watched
	// interactive launch non-blocking on tool-permission prompts, e.g.
	// "--dangerously-skip-permissions" for claude. Empty means the harness needs
	// nothing here, either because it has no such prompt (pi) or because its
	// headless template already handles approval (codex, opencode). Injected by
	// autonomyBypass instead of a hardcoded claude-specific flag.
	SkipPermissions string `toml:"skip_permissions"`
}

type harnessOverride struct {
	Name                string  `toml:"name"`
	Glob                *string `toml:"glob"`
	IDRe                *string `toml:"id_regex"`
	Format              *string `toml:"format"`
	DB                  *string `toml:"db"`
	Resume              *string `toml:"resume"`
	ResumeInput         *string `toml:"resume_input"`
	ResumeInputHeadless *string `toml:"resume_input_headless"`
	Launch              *string `toml:"launch"`
	LaunchHeadless      *string `toml:"launch_headless"`
	Args                *string `toml:"args"`
	WaitingRe           *string `toml:"waiting_re"`
	SkipPermissions     *string `toml:"skip_permissions"`
}

func (h harnessOverride) toHarness() Harness {
	return applyHarnessOverride(Harness{Name: h.Name}, h)
}

func applyHarnessOverride(base Harness, over harnessOverride) Harness {
	for _, f := range []struct {
		dst *string
		src *string
	}{
		{&base.Glob, over.Glob},
		{&base.IDRe, over.IDRe},
		{&base.Format, over.Format},
		{&base.DB, over.DB},
		{&base.Resume, over.Resume},
		{&base.ResumeInput, over.ResumeInput},
		{&base.ResumeInputHeadless, over.ResumeInputHeadless},
		{&base.Launch, over.Launch},
		{&base.LaunchHeadless, over.LaunchHeadless},
		{&base.Args, over.Args},
		{&base.WaitingRe, over.WaitingRe},
		{&base.SkipPermissions, over.SkipPermissions},
	} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
	return base
}

// Host is another machine whose sessions ax federates into the picker. ax runs
// `<transport> <ax> list --json` to read it, so a host needs ax installed and
// must be reachable by the transport (no daemon, no listening port: it rides ssh
// or any "run argv over there" command).
type Host struct {
	// Name labels the host in the picker's HOST column (e.g. "laptop", "vm").
	Name string `toml:"name"`
	// Transport is the command prefix that runs argv on the host, e.g.
	// "ssh user@box", "ssh -J jump box", or "kubectl exec -n ns pod --". ax
	// appends `<ax> list --json` (and, when attaching, `ax attach <id>`).
	Transport string `toml:"transport"`
	// Ax is the ax binary path on the host. Empty means "ax" (on PATH).
	Ax string `toml:"ax"`
	// RawArgv marks a transport that passes argv verbatim to the remote command
	// (kubectl exec ... --, docker exec) instead of re-parsing it through a
	// remote shell (ssh). ax quotes values for shell transports and must not for
	// raw ones, where quotes would arrive as literal characters.
	RawArgv bool `toml:"raw_argv"`
	// Shell is the remote shell ax quotes for when it re-parses argv over an
	// ssh-style transport: "posix" (the default when empty) or "pwsh". A pwsh
	// remote (Windows sshd with DefaultShell = pwsh) does not honor the POSIX
	// embedded-quote escape, so ax must quote such hosts with PowerShell's
	// doubled-quote form instead. Ignored when RawArgv is set (argv is verbatim).
	Shell string `toml:"shell"`
	// Headless marks a host that must launch harnesses headless: its interactive
	// launcher/mux is not wired (e.g. a Windows host, whose holder placement is
	// unverified), so a remote launch to it adds --headless to run the screenless
	// job form instead of hanging on an unwired interactive holder. Default false
	// launches remote interactive via the host's own launcher.
	Headless bool `toml:"headless"`
}

// Config is the full harness registry plus hosts and picker layout.
type Config struct {
	Harnesses []Harness `toml:"harness"`
	// Hosts are other machines to federate into the picker. Empty = local only.
	Hosts []Host `toml:"host"`
	// Columns chooses which picker columns to show and in what order, by key
	// (host, harness, win, model, age, ctx, cost, dir, title). Empty uses the
	// default order. Unknown keys are ignored.
	Columns []string `toml:"columns"`
	// ColumnDefaults set per-column default width/visibility/order for the picker's
	// column-management modal, keyed by the column's stable key. Configured as
	// repeated [[column]] tables; the array order is the default horizontal order
	// for the columns it names. Fields left unset fall back to the built-in default
	// for that column. This is the baseline the modal's "reset" (r) reverts to;
	// unknown keys are ignored. See ColumnDefault.
	ColumnDefaults []ColumnDefault `toml:"column"`
	// ColWidths overrides per-column display width at render time, applied by the
	// picker's column modal. Runtime only (never parsed from TOML), keyed by column
	// key; a key absent here uses the registry default width. See internal/view.
	ColWidths map[string]int `toml:"-"`
	// Keys rebinds picker actions, keyed by action name (see internal/keys). A
	// value is one key or a list, e.g. down = "j" or open = ["enter", "l"].
	// Unlisted actions keep their defaults.
	Keys map[string]StringList `toml:"keys"`
	// Shell is the command ax spawns a harness through, e.g. "sh -c" (the Unix
	// default), PowerShell on Windows, or "zsh -lic" for a login+interactive shell
	// that sets up your PATH (mise, nvm, asdf, ...) when ax's environment does not
	// already have it. The harness command is appended as the final argument.
	// Empty means the platform default.
	Shell string `toml:"shell"`
	// BehaviorsDir and RecipesDir are machine-local user content paths for the
	// compose flow. They are stored raw; callers expand a leading ~ at use sites.
	BehaviorsDir string `toml:"behaviors_dir"`
	RecipesDir   string `toml:"recipes_dir"`
	// Policy is the mechanical fence on what a session may launch: an allowed
	// harness list and an allowed/denied model list. ax refuses a launch outside it.
	Policy Policy `toml:"policy"`
	// Notify configures outbound lifecycle-event hooks so a human away from the
	// picker is reached with no daemon. Two TOML forms:
	//
	//   notify = "bell"          # string shorthand, fires on attention states
	//   [notify]                 # table form, maps event name -> command template
	//   run-success = "ax send {run} done"
	//
	// The string shorthand fires on needs-you and done-review only (back-compat).
	// "" (default) is off; "bell" and "tmux" are built-in shortcuts; anything
	// else is a template with {id} {state} {event} {summary} {name} {run}
	// placeholders ({group} is a deprecated alias for {run}; see internal/notify).
	Notify notify.Config `toml:"notify"`
	// Fence tunes the read-only capability fence (see internal/fence). Optional;
	// the zero value is the built-in allowlist with fail-closed behavior.
	Fence Fence `toml:"fence"`
	// Mux selects the terminal multiplexer backend session windows live in:
	// "tmux", "zellij", "process", or "none". Empty means the platform default
	// (tmux on Unix, process on Windows). zellij is weaker at per-pane queries than
	// tmux; "process" runs each session as a plain OS subprocess (no multiplexer)
	// with an input side channel for `ax send`; see internal/mux.
	Mux string `toml:"mux"`
	// MuxPrefix is the ax namespace prefix stamped on every window, session, or
	// tab name a real multiplexer backend (tmux, zellij) creates, so ax's own
	// windows are visually distinct from the user's in the native tmux status bar
	// or zellij tab bar and bulk-selecting/killing every ax-managed one is a
	// simple prefix match. "" (unset, the default) resolves to "ax:"; "off"
	// disables prefixing entirely (e.g. for scripts written against pre-prefix
	// window names); any other value is used as the literal prefix. Ignored by
	// the process and none backends, which have no window/session/tab to name.
	MuxPrefix string `toml:"mux_prefix"`
	// MuxGroup names the label key whose value a session window is grouped under:
	// every window sharing that label value is born in one mux session named
	// "<mux_prefix><value>", so a project's (or workstream's) sessions cluster into
	// one attachable, bulk-killable mux session. "off" (the default) and "" disable
	// grouping, keeping today's flat behavior where a window opens in the current
	// mux session. "project" groups by the auto-seeded project label; any other
	// value is treated as a label key and groups by that label's value. Only the
	// tmux backend with an active $TMUX honors it; every other backend (zellij,
	// process, none) and a session with no such label fall back to flat placement.
	MuxGroup string `toml:"mux_group"`
	// HoldBackend selects the session holder that keeps a harness alive across
	// window closes and detaches: "native" (default; ax's own holder inside
	// `ax run`, no external binary), "dtach" (the pre-native external dtach
	// wrapper, kept as a field fallback), or "none" (no holder: closing a
	// session's window kills it). See internal/hold.
	HoldBackend string `toml:"hold_backend"`
	// DetachPrefix rebinds the attach client's detach chord prefix: any
	// "ctrl-<letter>" (or the caret form "^b"). "" means the default ctrl-a.
	// The prefix arms the chord and never reaches the harness on its own;
	// pressing it twice sends one literal prefix through. See internal/hold.
	DetachPrefix string `toml:"detach_prefix"`
	// DetachKey rebinds the chord's command letter: a bare letter ("d" by
	// default, so the chord is ctrl-a then d). The pre-chord "ctrl-<letter>"
	// and caret forms are accepted as their letter. ctrl-\ stays as an
	// always-on detach fallback. See internal/hold.
	DetachKey string `toml:"detach_key"`
	// MenuKey rebinds the menu chord's command letter: a bare letter ("a" by
	// default, so the chord is ctrl-a then a). The menu chord detaches (the held
	// session survives, exactly like detach) and reopens the ax picker in the
	// same terminal, so you can hop between sessions without a multiplexer. It
	// shares detach_prefix. See internal/hold.
	MenuKey string `toml:"menu_key"`
	// Metrics gates the Prometheus textfile export written at run conclusion.
	Metrics Metrics `toml:"metrics"`
	// Retention is machine-local lifecycle cleanup policy. It is deliberately
	// excluded from config profiles/sync, like hosts and machine paths.
	Retention Retention `toml:"retention"`
	// Binds are user keybindings for the picker: press the leader key (see
	// internal/keys.Bind, default "`"), then Key, to run Run as a shell command.
	// No built-in defaults; empty means the leader does nothing.
	Binds []Bind `toml:"bind"`
	// DefaultHarness is the harness to use when the first positional argument is
	// not a known verb, harness name, or PATH extension. Set it to run prompts
	// directly: `ax "fix the bug"` becomes `ax <default_harness> "fix the bug"`.
	// Empty (default) means no shortcut: an unrecognized first arg is an error.
	// Known verbs and harness names always take precedence.
	DefaultHarness string `toml:"default_harness"`
	// Offline, when true, blocks every outbound network call ax can make. The only
	// ax-initiated call today is `ax models update` (fetches models.dev); with
	// offline set, that command exits with an error instead of contacting the
	// network. ax degrades gracefully to bundled or previously cached model data.
	// Also honored via AX_OFFLINE=1 (any non-empty value) in the environment.
	Offline bool `toml:"offline"`
}

// Retention controls lifecycle cleanup. "Archive" hides sessions from default
// views; worker reaping kills only the resident process. Neither deletes
// transcripts or metadata.
type Retention struct {
	AutoRetire           bool   `toml:"auto_retire"`
	RetainAfter          string `toml:"retain_after"`
	PruneCrashed         bool   `toml:"prune_crashed"`
	ReapConcludedWorkers bool   `toml:"reap_concluded_workers"`
	ReapAfter            string `toml:"reap_after"`
}

type retentionOverride struct {
	AutoRetire           *bool   `toml:"auto_retire"`
	RetainAfter          *string `toml:"retain_after"`
	PruneCrashed         *bool   `toml:"prune_crashed"`
	ReapConcludedWorkers *bool   `toml:"reap_concluded_workers"`
	ReapAfter            *string `toml:"reap_after"`
}

// RetainDuration parses retain_after, falling back to the safe default.
func (r Retention) RetainDuration() time.Duration {
	d, err := time.ParseDuration(r.RetainAfter)
	if err != nil || d < 0 {
		return 10 * time.Minute
	}
	return d
}

// ReapDelay parses reap_after, falling back to the dogfooding-safe default.
func (r Retention) ReapDelay() time.Duration {
	d, err := time.ParseDuration(r.ReapAfter)
	if err != nil || d < 0 {
		return 60 * time.Second
	}
	return d
}

// ColumnDefault sets a picker column's default width/visibility/order, keyed by
// the column's stable key (host, harness, model, age, ctx, cost, dir, title,
// ...). Configured as a repeated [[column]] table. The array order of the
// entries is the default horizontal order for the columns they name; columns not
// named keep their built-in order after them. Width 0 and Visible unset each fall
// back to the built-in default for that column. The picker's column-management
// modal treats this as the "reset" baseline.
type ColumnDefault struct {
	// Key is the column's stable key (matched case-insensitively; the legacy
	// "group" alias resolves to "run"). Required; an empty or unknown key is ignored.
	Key string `toml:"key"`
	// Width is the default display width; 0 (unset) keeps the built-in width.
	Width int `toml:"width"`
	// Visible is the default show/hide state; nil (unset) keeps the built-in state
	// (a column is built-in-visible when it is part of the default layout).
	Visible *bool `toml:"visible"`
}

// Bind is one user keybinding (`[[bind]]`): pressing Key after the picker's
// leader key runs Run as a shell command, with the TUI suspended around it so
// an interactive command (an editor, a pager) can own the terminal. Run's
// placeholders expand from the currently selected session ({id} {run} {dir}
// {transcript}) and, when File is set, a fixed path ({file}); see
// internal/finder/bind.go.
type Bind struct {
	Key  string `toml:"key"`
	Run  string `toml:"run"`
	File string `toml:"file"`
}

// Metrics configures `ax metrics`'s config-gated side effect: a Prometheus
// textfile export written at run conclusion (the done-gate), for node_exporter's
// textfile collector. `ax metrics --prom` always works ad hoc; this is only the
// on-conclusion auto-write.
type Metrics struct {
	// Textfile is the .prom file path to (re)write on every run conclusion.
	// "" (default) disables the write; ax metrics --prom keeps working regardless.
	Textfile string `toml:"textfile"`
}

// Fence configures the read-only capability fence a read-only session launches under.
type Fence struct {
	// Allow are extra leading commands the Bash allowlist permits, on top of the
	// built-in read-only set (e.g. allow = ["jq", "rg"] for a fenced session that
	// needs an extra query tool). Matched on the program name.
	//
	// WARNING: a command placed here inherits FULL write and exec capability. The
	// fence only checks the leading program name, so an allowed command that can
	// write or run code (make, go, python, sh, node, xargs, ...) defeats the fence
	// for that command: "make <target>" runs arbitrary shell and "go run"/"go test"
	// compile and execute code. Only list genuinely read-only tools here. To run a
	// project's own build or checks, delegate to a writable worker instead.
	Allow []string `toml:"allow"`
	// OnUnsupported chooses what happens when a fenced launch targets a harness ax
	// cannot fence (codex, pi, opencode): "refuse" (default, fail closed) or
	// "best-effort" (warn and launch unfenced). --fence best-effort overrides per
	// launch.
	OnUnsupported string `toml:"on_unsupported"`
}

// Policy is the launch allow/deny fence enforced by the control layer.
type Policy struct {
	// Harness is an allow-list of harness names; empty allows any configured.
	Harness []string `toml:"harness"`
	// Model constrains which models a launch may request.
	Model ModelPolicy `toml:"model"`
}

// ModelPolicy allows or denies models by glob (e.g. "*opus-max*"). Deny wins over
// allow; an empty policy allows everything.
type ModelPolicy struct {
	Allow []string `toml:"allow"`
	Deny  []string `toml:"deny"`
}

// StringList is a TOML value that accepts either a single string or a list of
// strings, so key bindings can be written as `down = "j"` or `open = ["j","k"]`.
type StringList []string

// UnmarshalTOML implements toml.Unmarshaler for the string-or-list form.
func (s *StringList) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		*s = StringList{x}
	case []any:
		out := make(StringList, 0, len(x))
		for _, e := range x {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		*s = out
	}
	return nil
}

// Default is the built-in registry. It works with no config file present.
func Default() Config {
	return Config{Retention: Retention{AutoRetire: true, RetainAfter: "10m", PruneCrashed: true, ReapConcludedWorkers: true, ReapAfter: "60s"}, Harnesses: []Harness{
		{
			Name:   "claude",
			Glob:   "~/.claude/projects/*/*.jsonl",
			IDRe:   `/(?P<id>[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`,
			Format: "claude",
			Resume: "cd {dir} && claude --resume {id} --model {model} {args}",
			// Resume-with-input: the same documented --resume mechanism, with the new
			// task as the initial prompt. Interactive continues the conversation
			// watched; -p runs it to completion for a scriptable job.
			ResumeInput:         "cd {dir} && claude --resume {id} --model {model} {args} {task}",
			ResumeInputHeadless: "cd {dir} && claude -p --resume {id} --model {model} {args} {task}",
			Launch:              "claude --session-id {newid} --model {model} --append-system-prompt {behavior} {args} {task}",
			LaunchHeadless:      "claude -p --session-id {newid} --model {model} --append-system-prompt {behavior} {args} {task}",
			// The folder-trust dialog and permission prompts block an interactive
			// session invisibly (--dangerously-skip-permissions does NOT skip
			// trust); matching them turns a silent hang into a "needs you" row and
			// a `waiting` follow event.
			WaitingRe: `one you trust\?|Do you trust|Do you want to`,
			// claude gates every tool call behind an interactive y/n prompt unless
			// bypassed; this is the only flag that skips it.
			SkipPermissions: "--dangerously-skip-permissions",
		},
		{
			Name:   "pi",
			Glob:   "~/.pi/agent/sessions/*/*.jsonl",
			IDRe:   `_(?P<id>[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`,
			Format: "pi",
			// --approve trusts the project's local files for this run, defusing pi's
			// "Trust project folder?" modal. That modal fires whenever the cwd holds
			// trust-requiring `.pi/` resources (settings.json, extensions, skills,
			// prompts, themes, SYSTEM.md, APPEND_SYSTEM.md) and, being a blocking menu,
			// freezes an ax-driven session exactly like codex's directory-trust dialog
			// (ax sees "idle", `ax send` can't answer it). resolveProjectTrusted short-
			// circuits on the flag (trustOverride) before any prompt, so it is the clean
			// per-launch bypass; a plain dir with no `.pi/` resources never prompts and
			// needs nothing. A global `defaultProjectTrust = "always"` would also work
			// but mutates the user's config for every dir; the flag scopes trust to the
			// launches ax itself makes.
			Resume: "cd {dir} && pi --approve --session {id} {args}",
			// pi mints its own id, but --session-id sets an exact one, creating it if
			// missing. Pre-minting a {newid} lets ax hold, heartbeat, and window-tag a
			// new pi session from the start (same as claude). pi also has
			// --append-system-prompt (for {behavior}) and -p for a headless run.
			Launch:         "pi --approve --session-id {newid} --model {model} --append-system-prompt {behavior} {args} {task}",
			LaunchHeadless: "pi --approve -p --session-id {newid} --model {model} --append-system-prompt {behavior} {args} {task}",
			// pi has no per-tool permission prompt at all: built-in tools run with the
			// pi process's own permissions unconditionally. Its only
			// interactive gate is the project-trust modal, handled by --approve in the
			// launch/resume templates above, so there is no extra flag to add here.
			SkipPermissions: "",
		},
		{
			Name:   "codex",
			Glob:   "~/.codex/sessions/*/*/*/rollout-*.jsonl",
			IDRe:   `-(?P<id>[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`,
			Format: "codex",
			// Current codex (0.142.x) gates every launch AND resume in an untrusted
			// directory behind an interactive "Do you trust this directory?" dialog,
			// and gates every model-run command behind a per-command approval prompt.
			// Both silently block an ax-driven session (ax sees "idle", `ax send`
			// never lands). The approval gate is defused by
			// `--sandbox danger-full-access -c approval_policy="never"` (same sandbox
			// rationale as the headless template below: the policy honored on both
			// API-key and ChatGPT accounts that never prompts). The trust gate is
			// independent of the sandbox and has no bypass flag: codex exposes no
			// global trust-disable setting, and a `-c projects."<dir>".trust_level`
			// override is NOT honored for the dialog (verified: the modal still
			// fires). The only non-interactive mechanism is to pre-write the persisted
			// trust block codex itself writes when you answer the dialog, which
			// pretrustDir does in ~/.codex/config.toml. resume carries the same
			// approval flags so a re-entered session stays drivable.
			Resume: "cd {dir} && codex resume {id} --sandbox danger-full-access -c approval_policy=\"never\" {args}",
			Launch: "codex --sandbox danger-full-access -c approval_policy=\"never\" {args} {task}",
			// Headless codex must run unattended with full write access, including
			// to .git (worktree add, commit). The `--dangerously-bypass-approvals-and-sandbox`
			// escape hatch is documented as "solely for environments that are
			// externally sandboxed": on a ChatGPT-plan account codex does not grant
			// it real full access, so the sandbox stays effectively read-only and
			// .git writes fail ("Operation not permitted" locking refs), stalling
			// the worker. Request the first-class policy instead: the
			// danger-full-access sandbox with an approval policy of never. That is
			// honored on both API-key and ChatGPT accounts and never prompts.
			LaunchHeadless: "codex exec --skip-git-repo-check --sandbox danger-full-access -c approval_policy=\"never\" {args} {task}",
			// codex's approval bypass rides in the Launch/Resume/Headless templates
			// above (a sandbox+approval-policy pair, not a single flag), so there is
			// no extra skip-permissions flag to add here.
			SkipPermissions: "",
		},
		{
			Name:   "opencode",
			DB:     "~/.local/share/opencode/opencode.db",
			Format: "opencode",
			Resume: "cd {dir} && opencode --session {id} {args}",
			// opencode's headless run is the `run` subcommand; it has no system-prompt
			// or session-id-create slot, so {behavior} folds into {task} and there is
			// no {newid}. `run` carries its own --dangerously-skip-permissions.
			Launch:         "opencode {args} {task}",
			LaunchHeadless: "opencode run --model {model} --dangerously-skip-permissions {args} {task}",
		},
	}}
}

// Path is where ax looks for the user config.
func Path() string {
	if x := os.Getenv("AX_CONFIG"); x != "" {
		return x
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "ax", "config.toml")
}

// contentDirDefault is the standard location for a machine-local content
// directory (recipes, behaviors): a sibling of the config file. It is the
// fallback ax uses when the user has not set recipes_dir/behaviors_dir, so a
// file dropped there (e.g. by the coordinator quickstart) is picked up with no
// config edit. ~ expansion is unnecessary: this is already an absolute path.
func contentDirDefault(sub string) string {
	return filepath.Join(filepath.Dir(Path()), sub)
}

// Load returns the defaults with any user config merged on top. A harness in the
// user config overlays a default of the same name field by field (each present
// field wins, including an explicit empty string, so config sync can clear a stale
// profile value); a new name is appended.
func Load() (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			cfg.BehaviorsDir = contentDirDefault("behaviors")
			cfg.RecipesDir = contentDirDefault("recipes")
			return cfg, nil
		}
		return cfg, err
	}
	var user Config
	if err := toml.Unmarshal(data, &user); err != nil {
		return cfg, err
	}
	var userHarnesses struct {
		Harnesses []harnessOverride `toml:"harness"`
	}
	if err := toml.Unmarshal(data, &userHarnesses); err != nil {
		return cfg, err
	}
	var userRetention struct {
		Retention retentionOverride `toml:"retention"`
	}
	if err := toml.Unmarshal(data, &userRetention); err != nil {
		return cfg, err
	}
	if len(user.Columns) > 0 {
		cfg.Columns = user.Columns
	}
	cfg.ColumnDefaults = user.ColumnDefaults // per-column default width/visible/order, user-defined
	cfg.Hosts = user.Hosts                   // hosts are purely user-defined (no built-in defaults)
	cfg.Keys = user.Keys                     // key rebinds too; keys.Build fills in the defaults
	cfg.Policy = user.Policy                 // launch allow/deny fence, user-defined
	cfg.Notify = user.Notify                 // lifecycle-event hooks, user-defined
	cfg.Fence = user.Fence                   // read-only fence tuning, user-defined
	// Machine-local content paths default to a sibling of the config file when
	// unset, so a recipe/behavior dropped there (coordinator quickstart) shows up
	// in the compose picker with no config edit. An explicit path always wins.
	if user.BehaviorsDir != "" {
		cfg.BehaviorsDir = user.BehaviorsDir
	} else {
		cfg.BehaviorsDir = contentDirDefault("behaviors")
	}
	if user.RecipesDir != "" {
		cfg.RecipesDir = user.RecipesDir
	} else {
		cfg.RecipesDir = contentDirDefault("recipes")
	}
	cfg.Mux = user.Mux                   // multiplexer backend selector, user-defined
	cfg.MuxPrefix = user.MuxPrefix       // ax namespace prefix for mux window/session/tab names, user-defined
	cfg.MuxGroup = user.MuxGroup         // per-key mux session grouping ("project"/label key), user-defined
	cfg.HoldBackend = user.HoldBackend   // session-holder backend selector, user-defined
	cfg.DetachPrefix = user.DetachPrefix // attach-client detach chord prefix rebind, user-defined
	cfg.DetachKey = user.DetachKey       // attach-client detach chord letter rebind, user-defined
	cfg.MenuKey = user.MenuKey           // attach-client menu chord letter rebind, user-defined
	cfg.Metrics = user.Metrics           // Prometheus textfile export gate, user-defined
	if userRetention.Retention.AutoRetire != nil {
		cfg.Retention.AutoRetire = *userRetention.Retention.AutoRetire
	}
	if userRetention.Retention.RetainAfter != nil {
		cfg.Retention.RetainAfter = *userRetention.Retention.RetainAfter
	}
	if userRetention.Retention.PruneCrashed != nil {
		cfg.Retention.PruneCrashed = *userRetention.Retention.PruneCrashed
	}
	if userRetention.Retention.ReapConcludedWorkers != nil {
		cfg.Retention.ReapConcludedWorkers = *userRetention.Retention.ReapConcludedWorkers
	}
	if userRetention.Retention.ReapAfter != nil {
		cfg.Retention.ReapAfter = *userRetention.Retention.ReapAfter
	}
	cfg.Binds = user.Binds                   // picker leader-key bindings, user-defined
	cfg.DefaultHarness = user.DefaultHarness // default harness for bare-prompt dispatch, user-defined
	cfg.Offline = user.Offline               // network opt-out, user-defined
	if user.Shell != "" {
		cfg.Shell = user.Shell
	}
	for _, h := range userHarnesses.Harnesses {
		replaced := false
		for i := range cfg.Harnesses {
			if cfg.Harnesses[i].Name == h.Name {
				cfg.Harnesses[i] = applyHarnessOverride(cfg.Harnesses[i], h)
				replaced = true
				break
			}
		}
		if !replaced {
			cfg.Harnesses = append(cfg.Harnesses, h.toHarness())
		}
	}
	return cfg, nil
}

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(p string) string {
	if len(p) > 0 && p[0] == '~' {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

// DirExists reports whether p is an existing directory (after ~ expansion). Used
// to detect a session whose recorded project folder was renamed or moved away.
func DirExists(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(ExpandHome(p))
	return err == nil && info.IsDir()
}
