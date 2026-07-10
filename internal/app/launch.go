package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/fence"
	"github.com/agentswitch-org/ax/internal/hold"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/runs"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/shell"
)

// fences are a run's hard mechanical limits. Zero
// means unset, except maxDepth whose effective default is 1 (flat).
type fences struct {
	maxCost    float64
	maxTokens  float64
	maxWorkers int
	maxDepth   int
	timeout    time.Duration
	// writeGlobs is the write scope: the path globs a fenced session may write
	// into, from repeatable --write. A non-empty scope engages the capability fence
	// (internal/fence): file-write tools are dropped from the deny list and the
	// fence-check hook allows a Write/Edit whose target matches one of the globs,
	// blocking everything else (and all mutating shell). Per-session: NOT injected
	// into a child's env, so a child a fenced session launches is writable.
	writeGlobs []string
	// noWrite engages the fence with an EMPTY write scope (from --no-write): zero
	// file writes are allowed. Mutually exclusive with writeGlobs.
	noWrite bool
	// noSubagents bars the session from the sub-agent spawn tools (Task/TaskCreate/
	// Agent), so it must delegate via `ax claude ...`. A role-agnostic capability
	// (userland composes any role from it); from --no-subagents. Orthogonal to the
	// write scope. It is NOT injected into a child's env (axEnv shadows it), so a
	// worker keeps Task.
	noSubagents bool
}

// launchOpts is a parsed `ax <harness> "TASK" ...` invocation.
type launchOpts struct {
	task         string
	behavior     string
	behaviorText string
	model        string
	group        string
	name         string
	parent       string
	accept       string // accept check script; runs when the root tags --outcome success
	labels       []string
	fen          fences
	wait         bool
	unattend     bool
	headless     bool   // opt in to the screenless headless job form (`claude -p`); the ONLY way in
	api          bool   // opt in to ANTHROPIC_API_KEY (pay-as-you-go) billing; default is subscription OAuth
	closeOnDone  bool   // close the window and end the session when the task concludes, instead of halting in the done state
	keepLive     bool   // opt out of delayed reaping after a parented task worker concludes
	keepLiveFor  string // keep-live lease duration (--keep-live-for DUR); implies keepLive, expires into reapable
	jsonOut      bool
	fenceMode    string // "best-effort" to launch an un-fenceable harness unfenced
	dir          string
	// cleanEnv starts the child from a minimal environment (a small allowlist,
	// the AX_* control vars, and the overrides below), so a worker never silently
	// inherits stray auth tokens or project config. envSet are explicit
	// KEY=VALUE overrides applied last (highest precedence).
	cleanEnv bool
	envSet   []string
	// auth selects the auth source: "subscription" (strip the Anthropic key env,
	// OAuth only), "api" (pass the ambient key through), or "env:VARNAME" (set the
	// key from a named variable, so a specific key is forced without exposing it).
	// Empty defaults to subscription unless --api set it to "api".
	auth   string
	effort string   // reasoning effort level (low, medium, high, xhigh, max, ...)
	host   string   // --host NAME: rerun this launch on the named host, id host-qualified
	hflags []string // after `--`
	// self-propel: the outer loop that re-invokes an idle inline (pi/codex)
	// coordinator until the project is done, waiting on a human, or capped. Opt-in
	// via --self-propel; meaningless (and refused) for a harness that sustains its
	// own agent loop (claude). See internal/propel.
	selfPropel    bool
	propelPrompt  string        // --propel-prompt: the continue-prompt injected each idle turn
	propelDone    string        // --propel-until / --done-check: shell cmd, exit 0 => project complete
	propelMaxIdle int           // --max-idle-turns: consecutive no-progress turns before stopping
	propelBackoff time.Duration // --propel-backoff: delay before re-injecting an idle session
	propelWatch   string        // --propel-watch: file whose mtime change counts as progress
}

// launchCtx carries the run-identity a restart pins onto a relaunch. For a normal
// launch it is the zero value and runLaunch derives group/parent/origin from the
// environment as before; when fromRestart is set, the persisted values are used
// verbatim (including an empty parent for a restarted root), so the reconstructed
// session slots back into its original run.
type launchCtx struct {
	fromRestart bool
	group       string
	parent      string
	origin      string
}

// Launch is the non-interactive launcher `ax <harness> "TASK"`: it resolves the
// behavior, enforces the policy and depth/worker fences, mints a session id,
// writes the metadata sidecar, and starts the harness (watched under tmux, or a
// headless job). It prints the session id (and group) so automation can capture
// them. This is the spawn primitive the control layer is built on.
func (a App) Launch(harness string, argv []string) {
	o, err := parseLaunch(argv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(2)
	}
	// A host target reruns the launch on that host and surfaces the remote id
	// host-qualified, before anything local is minted.
	if o.host != "" {
		a.remoteLaunch(harness, o)
		return
	}
	a.runLaunch(harness, o, launchCtx{})
}

// remoteLaunch reruns `<harness> "TASK" ...` on o.host over its transport and
// re-emits the remote session's id host-qualified (host/id), so the caller can
// immediately `ax read/wait/result host/id`. Nothing is minted locally: the
// remote host is the source of truth for the session. The task streams over the
// transport's stdin via --task-file - so a multi-line or special-char prompt
// never touches the remote shell parser; the transport is pty-stripped and
// per-shell quoted through the Step-1 path so the {id,group} the remote prints
// with --json is captured clean. A forced-headless host adds --headless.
func (a App) remoteLaunch(harness string, o launchOpts) {
	o.group, o.parent = inheritedGroupParent(o)
	h := lookupHost(o.host)
	axArgs := remoteLaunchArgv(harness, o, o.headless || h.Headless)
	prog, argv := transportArgv(remoteTransport(h, false), axArgs...)

	ctx, cancel := context.WithTimeout(context.Background(), remoteVerbTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, prog, argv...)
	c.Stdin = strings.NewReader(o.task)
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = os.Stderr // remote stderr passes straight through
	err := c.Run()
	if ctx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(os.Stderr, "ax: launch on %s timed out\n", o.host)
		os.Exit(255)
	}
	if err != nil {
		// The transport failed or the remote launch exited non-zero: pass its exit
		// code through and emit NO id, so a caller never captures a phantom handle.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() >= 0 {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "ax: launch on %s failed: %v\n", o.host, err)
		os.Exit(1)
	}
	id, group, perr := parseRemoteLaunch(out.String())
	if perr != nil {
		fmt.Fprintf(os.Stderr, "ax: %v\n", perr)
		os.Exit(1)
	}
	a.printLaunched(o.host+"/"+id, group, o.jsonOut)
}

// remoteLaunchArgv builds the ax argv that reruns this launch on a host: the
// harness verb, --task-file - (the task streams over the transport's stdin, so a
// multi-line or special-char prompt never reaches the remote shell parser),
// --json (so the remote prints a machine-parseable {id,group} the caller
// captures), and the forwarded run/label/model/behavior/effort/fence/api/dir
// flags. --host is dropped (we are already on the host) and --headless is added
// when the host forces headless (its interactive launcher is unwired). Pure so a
// test can assert on the argv without executing anything.
func remoteLaunchArgv(harness string, o launchOpts, headless bool) []string {
	args := []string{harness, "--task-file", "-", "--json"}
	if headless {
		args = append(args, "--headless")
	}
	if o.group != "" {
		args = append(args, "--run", o.group)
	}
	if o.parent != "" {
		args = append(args, "--parent", o.parent)
	}
	for _, l := range o.labels {
		args = append(args, "--label", l)
	}
	if o.model != "" {
		args = append(args, "--model", o.model)
	}
	if o.behavior != "" {
		args = append(args, "--behavior", o.behavior)
	}
	if o.behaviorText != "" {
		args = append(args, "--behavior-text", o.behaviorText)
	}
	if o.effort != "" {
		args = append(args, "--effort", o.effort)
	}
	for _, g := range o.fen.writeGlobs {
		args = append(args, "--write", g)
	}
	if o.fen.noWrite {
		args = append(args, "--no-write")
	}
	if o.fen.noSubagents {
		args = append(args, "--no-subagents")
	}
	if o.fen.maxCost > 0 {
		args = append(args, "--max-cost", strconv.FormatFloat(o.fen.maxCost, 'g', -1, 64))
	}
	if o.fen.maxTokens > 0 {
		args = append(args, "--max-tokens", strconv.FormatFloat(o.fen.maxTokens, 'g', -1, 64))
	}
	if o.fen.maxWorkers > 0 {
		args = append(args, "--max-workers", strconv.Itoa(o.fen.maxWorkers))
	}
	if o.fen.maxDepth > 0 {
		args = append(args, "--max-depth", strconv.Itoa(o.fen.maxDepth))
	}
	if o.api {
		args = append(args, "--api")
	}
	if o.keepLiveFor != "" {
		args = append(args, "--keep-live-for", o.keepLiveFor)
	} else if o.keepLive {
		args = append(args, "--keep-live")
	}
	if o.dir != "" {
		args = append(args, "--dir", o.dir)
	}
	if len(o.hflags) > 0 {
		args = append(args, "--")
		args = append(args, o.hflags...)
	}
	return args
}

// parseRemoteLaunch extracts the {"id":..,"group":..} a remote `--json` launch
// prints, scanning lines and tolerating surrounding noise (a pty can echo or
// CR-pad the line even with -t stripped). It slices from the first '{' to the
// last '}' on a line so trailing carriage returns do not defeat the unmarshal.
func parseRemoteLaunch(out string) (id, group string, err error) {
	for _, line := range strings.Split(out, "\n") {
		i := strings.IndexByte(line, '{')
		j := strings.LastIndexByte(line, '}')
		if i < 0 || j < i {
			continue
		}
		var rec struct {
			ID    string `json:"id"`
			Group string `json:"group"`
		}
		if json.Unmarshal([]byte(line[i:j+1]), &rec) == nil && rec.ID != "" {
			return rec.ID, rec.Group, nil
		}
	}
	return "", "", fmt.Errorf("no launch id in remote output")
}

// runLaunch executes a parsed launch. ctx pins the run identity when a restart
// reconstructs a session (see launchCtx); for a fresh launch it is the zero value
// and group/parent/origin come from the environment.
func (a App) runLaunch(harness string, o launchOpts, ctx launchCtx) {
	cfg, _ := config.Load()

	var h config.Harness
	for _, c := range cfg.Harnesses {
		if c.Name == harness {
			h = c
		}
	}
	if h.Name == "" {
		fmt.Fprintf(os.Stderr, "ax: unknown harness %q\n", harness)
		os.Exit(2)
	}

	// Self-propel is the outer loop for an inline harness that does one burst per
	// turn and then stops (pi, codex). A harness that sustains its own agent loop
	// (claude) needs no pump, so refuse the flag for it rather than persist a
	// no-op. A propelled coordinator must also stay live across turns, so it
	// implies --keep-live (a self-reaping worker would defeat the loop).
	if o.selfPropel {
		if h.Format != "pi" && h.Format != "codex" {
			fmt.Fprintf(os.Stderr, "ax: --self-propel is only supported for inline harnesses (pi, codex); ignoring it for %q\n", harness)
			o.selfPropel = false
		} else {
			o.keepLive = true
		}
	}

	// Policy fence: refuse a harness or model the config walls off.
	if err := policyAllows(cfg, harness, o.model); err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(3)
	}

	// A launch from inside a session inherits its run (group) and becomes its
	// child unless overridden; a launch by a human starts a fresh group. A restart
	// instead pins the persisted identity verbatim, so the reconstructed session
	// rejoins its original run rather than starting a new one.
	var group, parent, origin string
	if ctx.fromRestart {
		group, parent, origin = ctx.group, ctx.parent, ctx.origin
	} else {
		group, parent = inheritedGroupParent(o)
		if group == "" {
			group = shortID()
		}
		origin = "agent"
		if parent == "" {
			origin = "human"
		}
	}

	// Depth and worker fences are checked here, at the moment of launch, since
	// there is no daemon. Depth is inherited+1; workers are counted in the group.
	depth := envInt("AX_DEPTH", -1) + 1
	maxDepth := o.fen.maxDepth
	if maxDepth == 0 {
		maxDepth = envInt("AX_MAX_DEPTH", 1)
	}
	if depth > maxDepth {
		fmt.Fprintf(os.Stderr, "ax: refused: depth %d exceeds --max-depth %d\n", depth, maxDepth)
		os.Exit(4)
	}
	if mw := firstNonZero(o.fen.maxWorkers, envInt("AX_MAX_WORKERS", 0)); mw > 0 {
		if n := groupLiveCount(cfg, group); n >= mw {
			fmt.Fprintf(os.Stderr, "ax: refused: %d live workers in run %q at --max-workers %d\n", n, group, mw)
			os.Exit(4)
		}
	}

	// Default the workspace to the caller's cwd. The tmux path would otherwise
	// open the window in the tmux session's start directory, so a worker launched
	// from ~/src/proj would silently run its task somewhere else.
	if o.dir == "" {
		o.dir, _ = os.Getwd()
	}

	id := newUUID()
	behavior := o.behaviorText
	if o.behavior != "" {
		var err error
		behavior, err = resolveBehavior(o.behavior)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ax:", err)
			os.Exit(2)
		}
	}
	// The write capability fence is opt-in via --write/--no-write (parsed into
	// o.fen.writeGlobs/o.fen.noWrite); ax never infers it from the behavior text.

	// Capture the launch spec now, from the parsed opts and the resolved run
	// identity, BEFORE a slot-less harness folds the behavior into the task (which
	// mutates o.behavior/o.task below). This is what `ax restart` reconstructs from.
	spec := specFromOpts(harness, o, group, parent, origin)

	task := o.task

	// Freeform labels flow down the run: the child inherits the launching
	// session's labels (so a parent's `project=blog` lands on every child).
	// Explicit `--label`s override an inherited key. A parented child with no
	// explicit role gets role=worker; inherited roles still never flow down.
	inherited := inheritedLabels(parent)
	labels := childLabels(inherited, o.labels, parent)
	labels = seedProjectLabel(labels, o.dir)

	// Two independent axes (auth and billing).
	//
	// Execution mode: interactive/watched (default) runs the harness's live,
	// steerable form under the holder, so ANY session ax launches, including a
	// --wait/--unattended job, is attachable and shows its live harness TUI when
	// you drop into it. Headless (job) runs the screenless `claude -p` form to
	// completion and exits with NO attachable TUI: dropping into it shows a blank
	// screen. --wait/--unattended used to imply headless, which is why attaching to
	// a job showed nothing; that is gone. Headless is now opt-in ONLY via an
	// explicit --headless, NEVER --wait/--unattended and NEVER the silent default:
	// the headless form prefers ANTHROPIC_API_KEY, so a silent default billed
	// subscription users API credits. --wait/--unattended mean only "the parent
	// blocks on the job" and "the job runs unattended (no human)"; they run the
	// interactive form under the holder just like the default. A harness with no
	// {behavior} slot folds the behavior into the task.
	//
	// Auth/billing is the other axis, handled below at cmd time: the default strips
	// ANTHROPIC_API_KEY/AUTH_TOKEN from the child so a launch runs on the user's
	// subscription OAuth with zero credit burn; --api opts in to pay-as-you-go.
	tmpl, mode := launchMode(h, o, task)
	if behavior != "" && !strings.Contains(tmpl, "{behavior}") {
		task = behavior + "\n\n" + task
		behavior = ""
	}
	hargs := strings.TrimSpace(h.Args + " " + strings.Join(o.hflags, " "))
	// Make an autonomous launch non-blocking on tool permissions. A watched
	// interactive session (the default now) would otherwise hang on per-tool
	// permission prompts exactly the way headless already bypasses them; inject the
	// harness's permission-bypass flag. A fenced launch is the exception: its fence
	// strips the bypass and installs its own hook (below), so skip it here.
	fenced := o.fen.noWrite || len(o.fen.writeGlobs) > 0
	if !fenced {
		if bp := autonomyBypass(h); bp != "" && !strings.Contains(" "+hargs+" ", " "+bp+" ") {
			hargs = strings.TrimSpace(hargs + " " + bp)
		}
	}
	if fenced {
		// Resolve each write glob to an absolute pattern (expand ~, join the
		// workspace dir, clean) and pre-create its literal directory prefix before
		// launch: the fenced session cannot mkdir, and the fence-check hook matches
		// against the absolute pattern. Skipped entirely for --no-write (no globs).
		for i, g := range o.fen.writeGlobs {
			p := config.ExpandHome(g)
			if !filepath.IsAbs(p) {
				p = filepath.Join(o.dir, p)
			}
			p = filepath.Clean(p)
			o.fen.writeGlobs[i] = p
			if dir := literalGlobDir(p); dir != "" && dir != string(filepath.Separator) {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					fmt.Fprintln(os.Stderr, "ax: could not create --write dir:", err)
					os.Exit(2)
				}
			}
		}
		bestEffort := o.fenceMode == "best-effort" || cfg.Fence.OnUnsupported == "best-effort"
		out, refuse, warn := fenceHargs(h.Format, hargs, bestEffort, o.fen.writeGlobs, o.fen.noSubagents)
		if refuse {
			fmt.Fprintf(os.Stderr, "ax: refused: harness %q cannot enforce a write fence; omit --write/--no-write to launch writable, or --fence best-effort to launch unfenced anyway\n", harness)
			os.Exit(3)
		}
		if warn != "" {
			fmt.Fprintln(os.Stderr, "ax:", warn)
		}
		hargs = out
	}
	cmd := fillTemplate(tmpl, map[string]string{
		"newid": id, "model": o.model, "behavior": behavior, "task": task, "args": hargs,
	})

	// The control-layer environment injected into the child (so a nested `ax` self-
	// registers into the run). Built here because the env choke point below re-injects
	// it verbatim under --clean-env, where env -i would otherwise wipe it.
	// Carry this session's OWN final labels (inherited + explicit overrides +
	// seeded project) into the child env, so a nested launch that cannot read this
	// session's sidecar still inherits an explicit --label (e.g. role=worker), not
	// just the labels this session itself inherited.
	env := axEnv(id, group, parent, depth, maxDepth, o.fen, labels)
	if o.unattend { // so `ax ask` inside returns a default instead of blocking
		env = append(env, "AX_UNATTENDED=1")
	}
	if o.accept != "" { // so `ax tag --outcome success` runs the accept check
		env = append(env, "AX_ACCEPT="+o.accept)
	}

	// Environment choke point: the auth-source axis (subscription strips the
	// Anthropic key env so the child runs on the user's OAuth with zero credit burn;
	// api passes the ambient key through; env:VAR forces a specific key), plus
	// --clean-env (a minimal base, so a worker never silently inherits stray tokens
	// or config) and explicit --env overrides. Expressed as a single env/shell prefix
	// so it takes effect uniformly on every launch path (watched, --wait, detached).
	cmd = applyEnvPolicy(cmd, o, h.Format, env)
	// A watched interactive session also hits Claude Code's workspace folder-trust
	// dialog (which the permission-bypass flag does NOT skip; only headless `-p`
	// skips it). Pre-accept it for the launch dir so the worker does not hang. This
	// is best-effort and only writes when the dir is not already trusted; a failure
	// falls back to the human answering the dialog (surfaced as "needs you").
	if mode == "interactive" {
		pretrustDir(h.Format, o.dir)
	}

	// A fresh root reusing a group name starts a NEW run: clear any old record so
	// this run's conclusion (and a --wait exit code) never reads the previous
	// run's outcome.
	if parent == "" && runs.Exists(group) {
		runs.Remove(group)
	}

	// Record the metadata sidecar before launch so `ax list` sees the run
	// immediately, even before the harness writes its first transcript line.
	meta.Save(id, meta.Meta{
		Name: o.name, Task: o.task, Group: group, Parent: parent, Origin: origin,
		Harness: harness, Dir: o.dir, Labels: labels, Mode: mode, Effort: o.effort,
		// Only a task-carrying watched worker concludes on done, so --close-on-done
		// on a headless job (which exits on its own) is a harmless no-op.
		//
		// A --wait/--unattended job now runs the INTERACTIVE form (so it is
		// attachable), which does NOT exit on its own the way `claude -p` did: its
		// TUI sits idle after the turn ends. So it MUST close on done, or --wait
		// would block forever on a live-but-idle process and a background
		// unattended job would linger as a stray idle session. Closing on done
		// keeps the job semantics intact: it concludes (done/success recorded, the
		// marker survives teardown so `ax read`/`ax result` still work), then the
		// holder tears down and the process exits, letting --wait return its code.
		// The teardown fires only AFTER the turn concludes, so the session is fully
		// attachable for its entire run.
		CloseOnDone: (o.closeOnDone || o.wait || o.unattend) && mode == "interactive",
		KeepLive:    o.keepLive,
		KeepUntil:   keepLiveDeadline(o),
		Spec:        spec,
	})

	// Print identity first so a caller (an agent, or a script) can capture it
	// regardless of what the run does next.
	a.printLaunched(id, group, o.jsonOut)

	// A harness whose chosen template has no {newid} slot (codex, opencode) mints
	// its own session id, so the minted id above is only a placeholder: the run
	// wrapper's --adopt mode discovers the real id from the index after launch and
	// migrates the heartbeat and meta sidecar to it, so read/fences/tree bind to
	// the session that actually exists.
	adoptAs := ""
	if !strings.Contains(tmpl, "{newid}") {
		adoptAs = harness
	}

	// Every launch runs under the `ax run` wrapper (heartbeat, working/idle, fence
	// polling when it is a root, run record on exit), so it is tracked and
	// followable. --wait blocks on it for a job/CI result; otherwise it runs in a
	// tmux window (or held by a detached holder when not in tmux).
	switch {
	case o.wait:
		a.runWait(id, cmd, env, group, adoptAs, o)
	case a.mux.Active():
		title := launchWindowTitle(o.name, harness, group)
		held := heldWindowCmd(id, cmd)
		if adoptAs != "" {
			held = heldAdoptCmd(id, adoptAs, cmd)
		}
		// Group the window into its project's (or configured key's) mux session,
		// computed from the labels already seeded above. "" (grouping off or no such
		// label) keeps the flat current-session placement.
		target := muxTargetFor(labels, cfg.MuxGroup)
		// Foreground for a human watching; background when fanned out or unattended.
		if err := a.mux.Open(o.dir, title, axEnvPrefix(env)+held, id, target, origin == "human" && !o.unattend); err != nil {
			// The id/group were already printed, so a caller may hold a handle to a
			// session whose window never opened; surface the failure rather than
			// leave it silent (a too-long command, a dead mux socket).
			fmt.Fprintf(os.Stderr, "ax: could not open session window: %v\n", err)
		}
	default:
		a.runDetached(id, cmd, env, o.dir, adoptAs)
	}
}

// launchMode selects the harness command template and the execution mode a
// launch runs in. Interactive (the default) runs the harness's live, steerable
// TUI under the holder, so the session is attachable and shows its live screen
// when a viewer drops into it. Headless runs the screenless `claude -p` job form,
// which exits on its own but has NO attachable TUI. Headless is opt-in ONLY via
// --headless: --wait and --unattended run the interactive form too (they mean the
// caller blocks / no human is present, NOT screenless), so every ax session,
// including a job, is attachable. A harness with no headless template, or a
// taskless launch, always stays interactive.
func launchMode(h config.Harness, o launchOpts, task string) (tmpl, mode string) {
	if h.LaunchHeadless != "" && task != "" && o.headless {
		return h.LaunchHeadless, "headless"
	}
	return h.Launch, "interactive"
}

// launchWindowTitle names the window a fresh launch opens in: --name when set,
// else the harness. The group always folds in ahead of that (a run always has
// one, minted if not given) so every window in a run clusters together in the
// window list even when --name is set, ahead of the mux backend's own ax
// namespace prefix (see internal/mux).
func launchWindowTitle(name, harness, group string) string {
	title := name
	if title == "" {
		title = harness
	}
	if group != "" {
		title = group + "/" + title
	}
	return title
}

func runWrapperArgs(id, cmd, adopt string) []string {
	args := []string{"run"}
	if adopt != "" {
		args = append(args, "--adopt", adopt)
	}
	return append(args, id, cmd)
}

func inheritedGroupParent(o launchOpts) (group, parent string) {
	group = o.group
	if group == "" {
		group = RunEnv()
	}
	parent = o.parent
	if parent == "" {
		parent = os.Getenv("AX_SESSION_ID")
	}
	return group, parent
}

// runWait runs a launch to completion under the `ax run` wrapper (so the job gets
// the same heartbeat, fence polling, and run record as any tracked session) and
// blocks until it finishes, then exits with a status that reflects the run: 0 when
// the root tagged success (or --accept passes), non-zero when a fence tripped or
// it crashed, else the harness's own exit code. This is the job / CI path.
func (a App) runWait(id, cmd string, env []string, group, adoptAs string, o launchOpts) {
	c := exec.Command(self(), runWrapperArgs(id, cmd, adoptAs)...)
	c.Env = append(os.Environ(), env...)
	if o.dir != "" {
		c.Dir = o.dir
	}
	c.Stdin = os.Stdin
	c.Stdout, c.Stderr = os.Stderr, os.Stderr // keep stdout clean (the id line)
	if err := c.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	select {
	case <-done:
	case <-timeoutCh(o.fen.timeout):
		// SIGTERM, not SIGKILL: the wrapper forwards it to the harness, escalates
		// itself, clears the heartbeat, and writes the run record. A SIGKILL here
		// would orphan the harness (it keeps running and billing) and leave a
		// stale heartbeat with no record of the run.
		if c.Process != nil {
			c.Process.Signal(syscall.SIGTERM)
		}
		cfg, _ := config.Load()
		a.cascadeKill(cfg, group) // the workers; the root goes via its wrapper
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			if c.Process != nil {
				c.Process.Kill() // last resort for a wrapper that won't die
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
		fmt.Fprintln(os.Stderr, "ax: timeout")
		os.Exit(5)
	}
	os.Exit(a.waitExit(group, o.accept, o.dir, c))
}

// waitExit turns a finished run into --wait's exit code.
func (a App) waitExit(group, accept, dir string, c *exec.Cmd) int {
	if accept != "" { // an explicit accept check is the authority
		ac := shell.Command(accept)
		ac.Dir = dir
		if out, err := ac.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "ax: not done: %s\n", strings.TrimSpace(string(out)))
			return 1
		}
		return 0
	}
	if rec, ok := runs.Load(group); ok {
		switch rec.Outcome {
		case runs.Success:
			return 0
		case runs.BudgetHit, runs.Crashed:
			return 1
		}
	}
	// gave_up or no record: fall back to the harness's own exit code.
	if c.ProcessState != nil {
		return c.ProcessState.ExitCode()
	}
	return 0
}

// runDetached starts a watched session in the background when not inside tmux,
// held so it persists and can be attached later. Native: the `ax run` holder is
// itself the persistence layer, spawned detached (setsid) with its socket
// listening from birth, so no wrapper binary is needed. dtach: the old `dtach
// -n` spawn. none (or no dtach): a plain background heartbeat wrapper, unheld.
func (a App) runDetached(id, cmd string, env []string, dir, adoptAs string) {
	runArgs := runWrapperArgs(id, cmd, adoptAs)
	if hold.Backend() == hold.BackendDtach {
		if dtach, ok := hold.Path(); ok {
			c := exec.Command(dtach, append([]string{"-n", hold.Sock(id), self()}, runArgs...)...)
			c.Env = append(os.Environ(), env...)
			c.Dir = dir
			if err := startAndReap(c); err != nil {
				axlog.Printf("run %s: detached dtach start: %v", id, err)
			}
			return
		}
	}
	c := exec.Command(self(), runArgs...)
	c.Env = append(os.Environ(), env...)
	c.Dir = dir
	if hold.Backend() == hold.BackendNative {
		setDetached(c) // its own session: survives this launcher and its terminal
	}
	if err := startAndReap(c); err != nil {
		axlog.Printf("run %s: detached start: %v", id, err)
	}
}

func (a App) printLaunched(id, group string, jsonOut bool) {
	if jsonOut {
		fmt.Printf("{\"id\":%q,\"group\":%q}\n", id, group)
		return
	}
	fmt.Printf("%s\t%s\n", id, group)
}

// resolveBehavior turns a --behavior PATH into system-prompt text. --behavior is
// path-only: a missing or unreadable path is a hard error, never inline text.
func resolveBehavior(b string) (string, error) {
	if b == "" {
		return "", nil
	}
	p := config.ExpandHome(b)
	info, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("--behavior %s: not found", b)
		}
		return "", fmt.Errorf("--behavior %s: read failed: %w", b, err)
	}
	if info.IsDir() {
		return resolveBehaviorDir(b, p)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("--behavior %s: read failed: not a regular file or directory", b)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("--behavior %s: read failed: %w", b, err)
	}
	return strings.TrimRight(string(data), "\n"), nil
}

type behaviorFile struct {
	rel string
	abs string
}

func resolveBehaviorDir(orig, root string) (string, error) {
	var files []behaviorFile
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		if skipBehaviorFile(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !utf8.ValidString(rel) {
			return fmt.Errorf("invalid UTF-8 path %q", rel)
		}
		files = append(files, behaviorFile{rel: rel, abs: p})
		return nil
	}); err != nil {
		return "", fmt.Errorf("--behavior %s: read failed: %w", orig, err)
	}
	if len(files) == 0 {
		return "", fmt.Errorf("--behavior %s: empty folder", orig)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	sections := make([]string, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f.abs)
		if err != nil {
			return "", fmt.Errorf("--behavior %s: %s: read failed: %w", orig, f.rel, err)
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return "", fmt.Errorf("--behavior %s: %s: contains NUL byte", orig, f.rel)
		}
		if !utf8.Valid(data) {
			return "", fmt.Errorf("--behavior %s: %s: invalid UTF-8", orig, f.rel)
		}
		sections = append(sections, fmt.Sprintf("--- path: %s ---\n\n%s", f.rel, strings.TrimRight(string(data), "\n")))
	}
	return strings.Join(sections, "\n"), nil
}

func skipBehaviorFile(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~")
}

// literalGlobDir returns the literal directory prefix of an absolute glob
// pattern: the path up to (not including) the first segment carrying a glob
// metacharacter (`*`, `?`, `[`). A pattern with no metacharacter is a literal
// file path, so its parent directory is returned. This is the deepest directory
// the launcher can safely pre-create for a fenced session (which cannot mkdir).
func literalGlobDir(pattern string) string {
	segs := strings.Split(pattern, string(filepath.Separator))
	for i, s := range segs {
		if strings.ContainsAny(s, "*?[") {
			return strings.Join(segs[:i], string(filepath.Separator))
		}
	}
	return filepath.Dir(pattern)
}

// fenceHargs applies the write capability fence to a harness's args slot: it
// strips the permission-bypass flag (a fence with a bypass is no fence) and
// appends the harness-specific fence flags from fence.Apply. It reports
// refuse=true when the harness cannot be fenced and best-effort is off (the
// caller exits 3), or warn!="" when best-effort downgrades an un-fenceable
// harness to an unfenced launch.
func fenceHargs(format, hargs string, bestEffort bool, writeGlobs []string, noSubagents bool) (out string, refuse bool, warn string) {
	extra, _, err := fence.Apply(format, fence.Options{HookCmd: self() + " fence-check", WriteGlobs: writeGlobs, NoSubagents: noSubagents})
	if err != nil {
		if errors.Is(err, fence.ErrUnsupported) && bestEffort {
			return hargs, false, "warning: write fence not enforceable for this harness; launching UNFENCED (--fence best-effort)"
		}
		return hargs, true, ""
	}
	out = stripBypass(hargs)
	for _, a := range extra {
		out = strings.TrimSpace(out + " " + shellQuote(a))
	}
	return out, false, ""
}

// stripBypass removes the permission-bypass flag from an args string, so a
// read-only launch can never carry --dangerously-skip-permissions past the fence.
func stripBypass(args string) string {
	fields := strings.Fields(args)
	out := fields[:0]
	for _, f := range fields {
		if f == "--dangerously-skip-permissions" {
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// autonomyBypass returns the permission-bypass flag to inject into an autonomous
// (non-fenced) launch so a watched session does not hang on per-tool permission
// prompts. The flag is per-harness (config.Harness.SkipPermissions): only claude
// gates tool calls behind an interactive prompt, so only claude gets a flag here.
// codex/opencode carry their own bypass in their headless templates and their
// interactive form is a human path, and pi has no such prompt at all, so they get
// nothing here.
func autonomyBypass(h config.Harness) string {
	return h.SkipPermissions
}

// anthropicAuth reports whether a harness authenticates against the Anthropic
// API-key env, so the subscription/API-billing axis applies to it. codex (OpenAI)
// and opencode (multi-provider) authenticate elsewhere, so stripping the Anthropic
// keys for them would be meaningless or surprising.
func anthropicAuth(format string) bool {
	return format == "claude" || format == "pi"
}

// cleanEnvKeep is the allowlist of ambient variables carried through --clean-env:
// the minimum a harness needs to actually run (a shell, a PATH, a terminal, a
// home). Everything else is dropped, so a clean launch never inherits stray auth
// tokens or project config. The AX_* control vars and any --env overrides are
// layered on top of this base.
var cleanEnvKeep = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "TZ", "TMPDIR"}

// resolveAuth reduces the auth flags to one source: an explicit --auth wins, else
// legacy --api maps to "api", else the default is "subscription".
func resolveAuth(o launchOpts) string {
	if o.auth != "" {
		return o.auth
	}
	if o.api {
		return "api"
	}
	return "subscription"
}

// applyEnvPolicy wraps a harness command with the launch's environment and auth
// policy. It is a single env/shell prefix, so it takes effect identically on every
// launch path (watched, --wait, detached), which all run the returned command
// under one shell. The auth axis only applies to Anthropic-key harnesses (claude,
// pi); codex/opencode authenticate elsewhere, so their key env is left untouched.
// axvars are the AX_* control vars, re-injected under --clean-env so a nested `ax`
// inside a clean worker still self-registers into the run.
func applyEnvPolicy(cmd string, o launchOpts, format string, axvars []string) string {
	auth := ""
	if anthropicAuth(format) {
		auth = resolveAuth(o)
	}
	if o.cleanEnv {
		return cleanEnvCmd(cmd, o, auth, axvars)
	}
	return inheritEnvCmd(cmd, o, auth)
}

// inheritEnvCmd applies the auth decision and any --env overrides on top of the
// inherited environment, as shell statements prefixed to the command (which the
// `ax run` wrapper shell-execs). Subscription unsets the Anthropic key env (the
// only way to guarantee OAuth: the key outranks it, headless `-p` always uses it,
// and interactive otherwise hangs on an approval prompt); env:VAR sets the key
// from a named variable; api leaves the ambient key in place.
func inheritEnvCmd(cmd string, o launchOpts, auth string) string {
	var ops []shell.Op
	switch auth {
	case "subscription":
		ops = append(ops, shell.Unset("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"))
	case "", "api":
		// pass the ambient key env through untouched
	default: // "env:VARNAME"
		v := strings.TrimPrefix(auth, "env:")
		ops = append(ops, shell.SetRef("ANTHROPIC_API_KEY", v), shell.Unset("ANTHROPIC_AUTH_TOKEN"))
	}
	for _, kv := range o.envSet {
		ops = append(ops, envSetOp(kv))
	}
	return shell.InheritEnv(ops, cmd)
}

// cleanEnvCmd starts the child from a minimal environment: `env -i` with the keep
// allowlist (carried from the wrapper's env), the AX_* control vars, the auth
// additions, and the --env overrides, then runs the command under `sh -c`. env -i
// already excludes the Anthropic key env, so subscription needs nothing here; api
// and env:VAR re-add the key explicitly.
func cleanEnvCmd(cmd string, o launchOpts, auth string, axvars []string) string {
	var ops []shell.Op
	for _, k := range cleanEnvKeep {
		ops = append(ops, shell.SetRef(k, k)) // carry the wrapper's value through
	}
	for _, kv := range axvars {
		ops = append(ops, envSetOp(kv))
	}
	switch auth {
	case "", "subscription":
		// env -i already dropped the key env; nothing to add
	case "api":
		ops = append(ops, shell.SetRef("ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"))
	default: // "env:VARNAME"
		v := strings.TrimPrefix(auth, "env:")
		ops = append(ops, shell.SetRef("ANTHROPIC_API_KEY", v))
	}
	for _, kv := range o.envSet {
		ops = append(ops, envSetOp(kv))
	}
	return shell.CleanEnv(ops, cmd)
}

// envSetOp turns a KEY=VALUE pair into a literal environment assignment op,
// splitting on the first '=' (as env/export do) so a value carrying '=' (or a
// newline, as an AX_LABELS set does) is preserved whole; the platform shell
// layer quotes the value.
func envSetOp(kv string) shell.Op {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return shell.SetLiteral(kv[:i], kv[i+1:])
	}
	return shell.SetLiteral(kv, "")
}

// keepLiveDeadline resolves the keep-live lease into an absolute deadline stored
// on the meta sidecar: now+DUR for --keep-live-for, or the zero time for an
// indefinite --keep-live (and for no keep-live at all). The wrapper and the reap
// re-check now against this deadline, so a lease expires into reapable even if
// the launching coordinator has gone away.
func keepLiveDeadline(o launchOpts) time.Time {
	if o.keepLiveFor == "" {
		return time.Time{}
	}
	d, err := time.ParseDuration(o.keepLiveFor)
	if err != nil || d <= 0 {
		return time.Time{}
	}
	return time.Now().Add(d)
}

// specFromOpts captures a launch's full input as a persisted spec, so `ax restart`
// can reconstruct the session. It reads the parsed opts and the resolved run
// identity; call it before a slot-less harness folds the behavior into the task.
func specFromOpts(harness string, o launchOpts, group, parent, origin string) *meta.Spec {
	sp := &meta.Spec{
		Harness: harness, Task: o.task, Behavior: o.behavior, BehaviorText: o.behaviorText, Model: o.model,
		Name: o.name, Dir: o.dir, Accept: o.accept, Labels: o.labels, HFlags: o.hflags,
		Group: group, Parent: parent, Origin: origin,
		Headless: o.headless, Wait: o.wait, Unattended: o.unattend, CloseOnDone: o.closeOnDone, KeepLive: o.keepLive, KeepLiveFor: o.keepLiveFor,
		WriteGlobs: o.fen.writeGlobs, NoWrite: o.fen.noWrite, FenceMode: o.fenceMode,
		CleanEnv: o.cleanEnv, Env: o.envSet, Auth: o.auth,
		MaxCost: o.fen.maxCost, MaxTokens: o.fen.maxTokens,
		MaxWorkers: o.fen.maxWorkers, MaxDepth: o.fen.maxDepth,
		Effort:        o.effort,
		SelfPropel:    o.selfPropel,
		PropelPrompt:  o.propelPrompt,
		PropelDone:    o.propelDone,
		PropelMaxIdle: o.propelMaxIdle,
		PropelWatch:   o.propelWatch,
	}
	if o.fen.timeout > 0 {
		sp.Timeout = o.fen.timeout.String()
	}
	if o.propelBackoff > 0 {
		sp.PropelBackoff = o.propelBackoff.String()
	}
	return sp
}

// optsFromSpec rebuilds a launchOpts from a persisted spec, the inverse of
// specFromOpts, for `ax restart`.
func optsFromSpec(sp *meta.Spec) launchOpts {
	var o launchOpts
	o.task, o.behavior, o.behaviorText, o.model = sp.Task, sp.Behavior, sp.BehaviorText, sp.Model
	o.name, o.dir, o.accept = sp.Name, sp.Dir, sp.Accept
	o.labels, o.hflags, o.group = sp.Labels, sp.HFlags, sp.Group
	o.headless, o.wait, o.unattend = sp.Headless, sp.Wait, sp.Unattended
	o.closeOnDone, o.keepLive, o.fenceMode = sp.CloseOnDone, sp.KeepLive, sp.FenceMode
	o.keepLiveFor = sp.KeepLiveFor
	o.fen.writeGlobs, o.fen.noWrite = sp.WriteGlobs, sp.NoWrite
	o.cleanEnv, o.envSet, o.auth = sp.CleanEnv, sp.Env, sp.Auth
	o.fen.maxCost, o.fen.maxTokens = sp.MaxCost, sp.MaxTokens
	o.fen.maxWorkers, o.fen.maxDepth = sp.MaxWorkers, sp.MaxDepth
	if sp.Timeout != "" {
		o.fen.timeout, _ = time.ParseDuration(sp.Timeout)
	}
	o.effort = sp.Effort
	o.selfPropel, o.propelPrompt, o.propelDone, o.propelMaxIdle = sp.SelfPropel, sp.PropelPrompt, sp.PropelDone, sp.PropelMaxIdle
	o.propelWatch = sp.PropelWatch
	if sp.PropelBackoff != "" {
		o.propelBackoff, _ = time.ParseDuration(sp.PropelBackoff)
	}
	return o
}

// pretrustDir best-effort pre-accepts a harness's workspace folder-trust dialog
// for dir. Claude and codex both gate a launch in an untrusted directory behind an
// interactive dialog with no bypass flag; the only non-interactive mechanism is to
// pre-seed the trust record the tool writes when you answer the dialog. It only
// writes when the dir is not already trusted, so an already-trusted project (the
// common case) is a no-op and never races the tool's own writes; a fresh worktree
// gets pre-trusted so a watched/driven worker there does not hang. Any error is
// non-fatal: the human answers the dialog once, surfaced as "needs you".
func pretrustDir(format, dir string) {
	if dir == "" {
		return
	}
	abs, err := filepath.Abs(config.ExpandHome(dir))
	if err != nil {
		return
	}
	// Both tools key trust by the RESOLVED path (on macOS /tmp is a symlink to
	// /private/tmp), so pre-trusting the unresolved form misses and the worker
	// hangs on the dialog anyway.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	switch format {
	case "claude":
		pretrustClaude(abs)
	case "codex":
		pretrustCodex(abs)
	}
}

// pretrustClaude sets projects[abs].hasTrustDialogAccepted in ~/.claude.json.
func pretrustClaude(abs string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	p := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return // no Claude config yet; let the first launch's dialog seed it
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return
	}
	projects := map[string]map[string]any{}
	if raw, ok := top["projects"]; ok {
		json.Unmarshal(raw, &projects)
	}
	proj := projects[abs]
	if proj == nil {
		proj = map[string]any{}
	}
	if t, _ := proj["hasTrustDialogAccepted"].(bool); t {
		return // already trusted: no write, no race
	}
	proj["hasTrustDialogAccepted"] = true
	projects[abs] = proj
	pj, err := json.Marshal(projects)
	if err != nil {
		return
	}
	top["projects"] = pj
	out, err := json.Marshal(top)
	if err != nil {
		return
	}
	tmp := p + ".ax.tmp"
	if os.WriteFile(tmp, out, 0o600) != nil {
		return
	}
	if os.Rename(tmp, p) == nil {
		axlog.Printf("pretrust: accepted workspace trust for %s in %s", abs, p)
	}
}

// pretrustCodex appends [projects."<abs>"] trust_level = "trusted" to
// ~/.codex/config.toml, the exact block codex persists when you answer its
// directory-trust dialog. Append-only and guarded by an existence check so it
// never disturbs the user's model/config settings or duplicates a table; codex
// reads it and skips the dialog. TOML basic-string keys escape backslash and
// quote (paths rarely contain either).
func pretrustCodex(abs string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".codex")
	p := filepath.Join(dir, "config.toml")
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(abs)
	header := `[projects."` + esc + `"]`
	if data, err := os.ReadFile(p); err == nil && strings.Contains(string(data), header) {
		return // already trusted: no write
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.WriteString("\n" + header + "\ntrust_level = \"trusted\"\n"); err == nil {
		axlog.Printf("pretrust: accepted codex directory trust for %s in %s", abs, p)
	}
}

// policyAllows enforces the [policy] fence: an allowed harness list and a model
// allow/deny (deny wins, glob-matched). An empty policy allows everything.
func policyAllows(cfg config.Config, harness, model string) error {
	p := cfg.Policy
	if len(p.Harness) > 0 && !contains(p.Harness, harness) {
		return fmt.Errorf("harness %q not in policy allow-list", harness)
	}
	if model == "" {
		return nil
	}
	for _, g := range p.Model.Deny {
		if ok, _ := path.Match(g, model); ok {
			return fmt.Errorf("model %q denied by policy", model)
		}
	}
	if len(p.Model.Allow) > 0 {
		for _, g := range p.Model.Allow {
			if ok, _ := path.Match(g, model); ok {
				return nil
			}
		}
		return fmt.Errorf("model %q not in policy allow-list", model)
	}
	return nil
}

// groupLiveCount counts live sessions in a group (the tree width the worker fence
// bounds).
func groupLiveCount(cfg config.Config, group string) int {
	snap := live.Snapshot()
	n := 0
	for _, s := range session.Index(cfg) {
		if s.Group == group && s.Host == "" {
			if e, ok := snap[s.ID]; ok && live.Running(e) {
				n++
			}
		}
	}
	return n
}

// axEnv is the control-layer environment injected into a launched session, so a
// nested `ax <harness>` self-registers into the run and self-governs the fences.
func axEnv(id, group, parent string, depth, maxDepth int, f fences, labels []string) []string {
	env := []string{"AX_SESSION_ID=" + id, "AX_DEPTH=" + strconv.Itoa(depth), "AX_MAX_DEPTH=" + strconv.Itoa(maxDepth)}
	if group != "" {
		// AX_GROUP is kept alongside AX_RUN, deprecated but still set, so anything
		// that only reads the old name keeps working.
		env = append(env, "AX_RUN="+group, "AX_GROUP="+group)
	}
	if parent != "" {
		env = append(env, "AX_PARENT="+parent)
	}
	// This session's own final labels, so they inherit down a nested launch even
	// when the child can't read this session's sidecar (a detached or remote run).
	if len(labels) > 0 {
		env = append(env, "AX_LABELS="+joinLabels(labels))
	}
	if f.maxCost > 0 {
		env = append(env, "AX_MAX_COST="+strconv.FormatFloat(f.maxCost, 'g', -1, 64))
	}
	if f.maxTokens > 0 {
		env = append(env, "AX_MAX_TOKENS="+strconv.FormatFloat(f.maxTokens, 'g', -1, 64))
	}
	if f.maxWorkers > 0 {
		env = append(env, "AX_MAX_WORKERS="+strconv.Itoa(f.maxWorkers))
	}
	if f.timeout > 0 {
		env = append(env, "AX_TIMEOUT="+f.timeout.String())
	}
	// The write scope, so the child's `ax fence-check` hook knows which globs a
	// fenced session may write into. When the launch is fenced (--write or
	// --no-write), AX_WRITE is set EXPLICITLY (newline-joined globs, empty for
	// --no-write) so an inherited parent AX_WRITE cannot leak into a differently-
	// fenced child; a non-fenced (writable) launch leaves it unset.
	if f.noWrite || len(f.writeGlobs) > 0 {
		env = append(env, "AX_WRITE="+strings.Join(f.writeGlobs, "\n"))
	}
	// AX_NO_SUBAGENTS is set UNCONDITIONALLY (1 when on, empty when off) so the
	// capability never inherits: a --no-subagents session that launches a worker
	// must let the worker Task, so an empty value here shadows any AX_NO_SUBAGENTS=1
	// leaked from os.Environ(). Role-agnostic: no AX_ROLE.
	if f.noSubagents {
		env = append(env, "AX_NO_SUBAGENTS=1")
	} else {
		env = append(env, "AX_NO_SUBAGENTS=")
	}
	return env
}

// inheritedLabels returns the labels a launched child inherits from its parent:
// the parent session's current labels (its meta sidecar), falling back to the
// AX_LABELS env so inheritance still flows down a nested launch whose child
// cannot read the parent's sidecar (a detached or remote run).
func inheritedLabels(parent string) []string {
	if parent != "" {
		if m := meta.Load(parent); len(m.Labels) > 0 {
			return m.Labels
		}
	}
	return splitLabels(os.Getenv("AX_LABELS"))
}

// childLabels folds a launched child's label set: the inherited parent labels,
// then the child's explicit `--label` edits, so an explicit label overrides an
// inherited one of the same key. For a spawned child (Parent not empty), core
// defaults role=worker only when this launch did not explicitly set/clear role.
// EditLabels keeps the set unique and one value per key.
//
// The "role" key is the one exception to inheritance: a role describes what
// one session IS (a lead, a worker, a reviewer), not what its whole subtree
// is, so it never flows down. Inheriting it would mislabel every child with
// its parent's role. A child's role comes only from an explicit
// --label role=... on its own launch; otherwise a parented child defaults to
// role=worker.
func childLabels(inherited []string, explicit []string, parent string) []string {
	out := make([]string, 0, len(inherited))
	for _, l := range inherited {
		if k, _, ok := strings.Cut(l, "="); ok && k == "role" {
			continue
		}
		out = append(out, l)
	}
	explicitRole := false
	for _, l := range explicit {
		if k, _, ok := strings.Cut(l, "="); ok && k == "role" {
			explicitRole = true
		}
		out = session.EditLabels(out, l)
	}
	if parent != "" && !explicitRole && session.LabelValue(out, "role") == "" {
		out = session.EditLabels(out, "role=worker")
	}
	return out
}

// muxTargetFor returns the mux session a window should be born in under the
// configured grouping key, or "" when grouping is off or the key is absent from
// this session's labels. muxGroup is the `mux_group` config value: "off" (and
// "", the default) disable grouping; any other value names a label key
// ("project" reads the auto-seeded project label; a custom key like "workstream"
// reads that label). A "" return means today's flat placement, which every
// backend that cannot honor a target already falls back to.
func muxTargetFor(labels []string, muxGroup string) string {
	switch muxGroup {
	case "", "off":
		return ""
	default:
		return session.LabelValue(labels, muxGroup)
	}
}

// seedProjectLabel fills in a "project" label when the folded set (parent
// inheritance + explicit --label) still has none: it derives one from the
// launch directory's git repository, preferring the origin remote's
// repository name (stable across worktrees and clones of the same project,
// where the toplevel directory's own name may differ) over the toplevel
// directory's base name, and falling back to dir's own base name outside a
// git repository altogether. An inherited or explicit project label always
// wins over this; it only fills a true gap, so a whole family of sessions in
// a project picks up the label without hand-tagging.
func seedProjectLabel(labels []string, dir string) []string {
	if session.LabelValue(labels, "project") != "" {
		return labels
	}
	if p := deriveProject(dir); p != "" {
		labels = session.EditLabels(labels, "project="+p)
	}
	return labels
}

// deriveProject derives a project name for dir: the origin remote's
// repository name when one is set, else the git toplevel directory's base
// name, else dir's own base name when it is not a git repository at all.
func deriveProject(dir string) string {
	if url := gitOriginURL(dir); url != "" {
		if name := repoNameFromURL(url); name != "" {
			return name
		}
	}
	if top := gitToplevel(dir); top != "" {
		return filepath.Base(top)
	}
	if dir != "" {
		return filepath.Base(dir)
	}
	return ""
}

// gitToplevel returns the absolute path of dir's git repository root, or ""
// when dir is not inside a git repository (or git is unavailable).
func gitToplevel(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitOriginURL returns dir's "origin" remote URL, or "" when dir is not a git
// repository or has no origin configured.
func gitOriginURL(dir string) string {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// repoNameFromURL extracts a repository name from a git remote URL, handling
// scp-like (git@host:org/repo.git), https, and ssh:// forms alike.
func repoNameFromURL(url string) string {
	url = strings.TrimRight(url, "/")
	url = strings.TrimSuffix(url, ".git")
	if i := strings.LastIndexAny(url, "/:"); i >= 0 && i+1 < len(url) {
		return url[i+1:]
	}
	return url
}

// joinLabels/splitLabels serialize a label set for the AX_LABELS env. Labels may
// contain spaces, so they are newline-joined (which survives shell-quoting).
func joinLabels(labels []string) string { return strings.Join(labels, "\n") }

func splitLabels(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// axEnvPrefix renders env assignments as a shell prefix for a tmux/dtach command
// (values shell-quoted), so the whole held chain inherits them. The platform
// layer picks the form (POSIX `K='v' cmd` / PowerShell `$env:K = 'v'; cmd`).
func axEnvPrefix(env []string) string { return shell.InlineEnv(env) }

// parseLaunch parses the launcher argv: a positional task (or "-" for stdin),
// the control flags, and harness flags after "--". --task-file <path> reads the
// task from a file (or "-" for stdin), so multi-line prompts skip shell-quoting.
func parseLaunch(argv []string) (launchOpts, error) {
	var o launchOpts
	var taskFile string
	behaviorSet := false
	behaviorTextSet := false
	need := func(i int) (string, error) {
		if i+1 >= len(argv) {
			return "", fmt.Errorf("%s needs a value", argv[i])
		}
		return argv[i+1], nil
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			o.hflags = append(o.hflags, argv[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			// Bare words join into one task, so an unquoted multi-word prompt is
			// taken whole instead of silently truncated to its first word.
			if o.task == "" {
				o.task = a
			} else {
				o.task += " " + a
			}
			continue
		}
		v, err := need(i)
		switch a {
		case "--behavior":
			behaviorSet = true
			o.behavior = v
		case "--behavior-text":
			behaviorTextSet = true
			o.behaviorText = v
		case "--model":
			o.model = v
		case "--run", "--group": // --group is a deprecated alias for --run
			o.group = v
		case "--name":
			o.name = v
		case "--parent":
			o.parent = v
		case "--label":
			o.labels = append(o.labels, v)
		case "--host":
			o.host = v
		case "--dir":
			o.dir = v
		case "--accept":
			o.accept = v
		case "--max-cost":
			o.fen.maxCost, _ = strconv.ParseFloat(v, 64)
		case "--max-tokens":
			o.fen.maxTokens, _ = strconv.ParseFloat(v, 64)
		case "--max-workers":
			o.fen.maxWorkers, _ = strconv.Atoi(v)
		case "--max-depth":
			o.fen.maxDepth, _ = strconv.Atoi(v)
		case "--timeout":
			o.fen.timeout, _ = time.ParseDuration(v)
		case "--wait":
			o.wait = true
			continue
		case "--unattended":
			o.unattend = true
			continue
		case "--interactive":
			continue
		case "--headless":
			o.headless = true
			continue
		case "--api":
			o.api = true
			continue
		case "--clean-env":
			o.cleanEnv = true
			continue
		case "--env":
			if !strings.Contains(v, "=") {
				return o, fmt.Errorf("--env expects KEY=VALUE, got %q", v)
			}
			if k, _, _ := strings.Cut(v, "="); !validEnvName(k) {
				return o, fmt.Errorf("--env: invalid variable name %q", k)
			}
			o.envSet = append(o.envSet, v)
		case "--auth":
			a, err := normalizeAuth(v)
			if err != nil {
				return o, err
			}
			o.auth = a
		case "--close-on-done":
			o.closeOnDone = true
			continue
		case "--keep-live":
			o.keepLive = true
			continue
		case "--keep-live-for":
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return o, fmt.Errorf("--keep-live-for: invalid duration %q", v)
			}
			o.keepLive = true
			o.keepLiveFor = v
		case "--write":
			o.fen.writeGlobs = append(o.fen.writeGlobs, v)
		case "--no-write":
			o.fen.noWrite = true
			continue
		case "--no-subagents":
			o.fen.noSubagents = true
			continue
		case "--fence":
			o.fenceMode = v
		case "--effort":
			o.effort = v
		case "--self-propel":
			o.selfPropel = true
			continue
		case "--propel-prompt":
			o.propelPrompt = v
		case "--propel-until", "--done-check":
			o.propelDone = v
		case "--max-idle-turns", "--propel-max-idle":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return o, fmt.Errorf("%s: invalid count %q", a, v)
			}
			o.propelMaxIdle = n
		case "--propel-backoff":
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return o, fmt.Errorf("--propel-backoff: invalid duration %q", v)
			}
			o.propelBackoff = d
		case "--propel-watch":
			o.propelWatch = v
		case "--json":
			o.jsonOut = true
			continue
		case "--task-file":
			taskFile = v
		default:
			return o, fmt.Errorf("unknown flag %q", a)
		}
		if err != nil {
			return o, err
		}
		i++ // consumed the value
	}
	if o.fen.noWrite && len(o.fen.writeGlobs) > 0 {
		return o, fmt.Errorf("--no-write and --write are mutually exclusive: --no-write grants no write scope, --write grants one")
	}
	if behaviorSet && behaviorTextSet {
		return o, fmt.Errorf("--behavior and --behavior-text are mutually exclusive")
	}
	if taskFile != "" && o.task != "" {
		return o, fmt.Errorf("--task-file and a positional task cannot both be given")
	}
	if taskFile != "" {
		t, err := readTaskFrom(taskFile)
		if err != nil {
			return o, fmt.Errorf("--task-file: %w", err)
		}
		o.task = t
	} else if o.task == "-" {
		t, _ := readTaskFrom("-")
		o.task = t
	}
	return o, nil
}

// readTaskFrom reads task text from a file path or "-" for stdin, trimming a
// trailing newline so a file ending in \n does not inject one into the prompt.
func readTaskFrom(src string) (string, error) {
	if src == "-" {
		var b strings.Builder
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			b.WriteString(sc.Text())
			b.WriteByte('\n')
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\n"), nil
}

// normalizeAuth validates and canonicalizes a --auth value: "subscription"/"sub"
// force OAuth, "api" uses the ambient key, and "env:VARNAME" forces the key from a
// named variable (the secret-safe way to pin a specific key, since only the name
// is persisted). Anything else is rejected.
func normalizeAuth(v string) (string, error) {
	switch v {
	case "subscription", "sub":
		return "subscription", nil
	case "api":
		return "api", nil
	}
	if name, ok := strings.CutPrefix(v, "env:"); ok {
		if !validEnvName(name) {
			return "", fmt.Errorf("--auth env: invalid variable name %q", name)
		}
		return "env:" + name, nil
	}
	return "", fmt.Errorf("--auth expects subscription|api|env:VARNAME, got %q", v)
}

// validEnvName reports whether s is a POSIX-ish environment variable name (so it
// can be safely spliced into a shell env prefix without quoting the name).
func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// shortID is a short run id for a run started without an explicit --run.
func shortID() string {
	u := newUUID()
	if len(u) >= 8 {
		return u[:8]
	}
	return u
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

func timeoutCh(d time.Duration) <-chan time.Time {
	if d <= 0 {
		return nil // never fires
	}
	return time.After(d)
}
