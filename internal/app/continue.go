package app

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/runs"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
)

// Continue resumes an existing session's context with a new task, delivered as
// input (`ax continue <id> "TASK"`): the reuse primitive between `ax send` (which
// needs the session already live in a tmux window) and a cold launch. It resumes
// the harness's own conversation (its documented resume mechanism) with the new
// prompt, tracked under the `ax run` wrapper like any launch and scriptable
// headless, keeping the same session identity. A harness with no resume-with-input
// form degrades gracefully with a message rather than doing the wrong thing.
func (a App) Continue(args []string) {
	id, o, err := parseContinue(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(2)
	}
	a.runContinue(resolveID(id), o)
}

// parseContinue splits `continue`'s argv into the leading session id and a launch
// spec: the id is the first bare word (like send/attach/restart), the rest is the
// new task plus flags, parsed by parseLaunch. It errors (rather than exiting) so
// the argument handling is unit-testable.
func parseContinue(args []string) (string, launchOpts, error) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return "", launchOpts{}, fmt.Errorf("usage: ax continue <id> \"TASK\" [flags]")
	}
	id := args[0]
	o, err := parseLaunch(args[1:])
	if err != nil {
		return "", launchOpts{}, err
	}
	if strings.TrimSpace(o.task) == "" {
		return "", launchOpts{}, fmt.Errorf("continue needs a task to deliver (ax continue <id> \"TASK\" or --task-file <path>)")
	}
	return id, o, nil
}

// errContinueUnsupported marks a harness with no resume-with-input form at all, so
// `ax continue` degrades gracefully to a "attach + send, or launch fresh" message
// instead of guessing a resume flag that would mint a new id or drop context.
var errContinueUnsupported = errors.New("harness has no documented resume-with-input form")

// resumeInputTemplate picks the resume-with-input template for the requested mode
// and reports how a harness supports (or does not support) `ax continue`:
//   - a template and mode ("interactive"|"headless") when supported;
//   - errContinueUnsupported when the harness cannot continue at all (both forms empty);
//   - a distinct error when only the headless form is missing, so the caller can
//     tell the user to drop --wait/--headless rather than give up.
func resumeInputTemplate(h config.Harness, headless bool) (tmpl, mode string, err error) {
	if h.ResumeInput == "" && h.ResumeInputHeadless == "" {
		return "", "", errContinueUnsupported
	}
	if headless {
		if h.ResumeInputHeadless == "" {
			return "", "", fmt.Errorf("harness %q has no headless resume-with-input form; run it watched (drop --wait/--headless)", h.Name)
		}
		return h.ResumeInputHeadless, "headless", nil
	}
	if h.ResumeInput == "" {
		return "", "", fmt.Errorf("harness %q supports only a headless continue; pass --wait", h.Name)
	}
	return h.ResumeInput, "interactive", nil
}

// runContinue resumes session id with a new task. It mirrors the launch dispatch
// (watched window, --wait job, or detached) but reuses the existing session id
// and transcript via the harness's resume-with-input template instead of minting
// a fresh session.
func (a App) runContinue(id string, o launchOpts) {
	cfg, _ := config.Load()

	// Resolve the session against the local index; a remote session is resumed on
	// its owner (over the transport), not from here.
	sessions := session.Index(cfg)
	var s session.Session
	found := false
	for _, x := range sessions {
		if x.ID == id && x.Host == "" {
			s, found = x, true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "ax: no local session %q\n", id)
		os.Exit(1)
	}

	// A live session normally takes input via `ax send`; continue reaches the
	// concluded/detached case send cannot. The exception is live-reuse: a
	// reuse-ready worker (live, task-concluded, keep-live, between turns) is
	// assigned a NEW task through continue, not raw send, so its task/wait/result
	// state is reset and the next `ax wait`/`ax result` track the new task instead
	// of the stale success. Raw send is steering only; it never resets that state,
	// which is exactly why it must not be the new-task channel.
	if e, ok := live.Snapshot()[id]; ok && live.Running(e) {
		m := meta.Load(id)
		r := state.ComputeAll(sessions)[id]
		if reuseReadyFacts(r, m.Mode, m.Task, m.KeepLive, m.KeepUntil, time.Now()) {
			if err := a.continueLiveReuse(id, m.Group, o); err != nil {
				fmt.Fprintln(os.Stderr, "ax:", err)
				os.Exit(1)
			}
			return
		}
		fmt.Fprintf(os.Stderr, "ax: session %q is live but not reuse-ready; use `ax send %s \"...\"` to steer its current task, or wait for it to conclude (a reuse target is a concluded keep-live worker between turns)\n", id, id)
		os.Exit(1)
	}

	var h config.Harness
	for _, c := range cfg.Harnesses {
		if c.Name == s.Harness {
			h = c
		}
	}
	if h.Name == "" {
		fmt.Fprintf(os.Stderr, "ax: unknown harness %q\n", s.Harness)
		os.Exit(2)
	}

	// Pick the resume-with-input template for the mode. A harness with none cannot
	// cleanly continue: degrade gracefully with a message instead of guessing a
	// resume flag that mints a new id or drops the prior context.
	headless := o.wait || o.unattend || o.headless
	tmpl, mode, err := resumeInputTemplate(h, headless)
	if err != nil {
		if errors.Is(err, errContinueUnsupported) {
			fmt.Fprintf(os.Stderr, "ax: harness %q does not support `ax continue` (no documented resume-with-input); attach it (`ax attach %s`) and use `ax send`, or start a fresh session with the prior context as a handoff\n", s.Harness, id)
		} else {
			fmt.Fprintln(os.Stderr, "ax:", err)
		}
		os.Exit(3)
	}

	// Move the transcript to where the harness will look for it in s.Dir (a session
	// stranded by a directory move would otherwise resume into "No conversation
	// found"), the same heal `ax attach` does.
	if err := session.Relocate(h, s); err != nil {
		axlog.Printf("continue %s: relocate: %v", id, err)
	}

	// The resumed session keeps its own id, run, and topology. Read them off the
	// sidecar so the continued turn stays inside the original run.
	m := meta.Load(id)
	group, parent, origin := m.Group, m.Parent, m.Origin
	if origin == "" {
		origin = "human"
	}

	model := o.model
	if model == "" {
		model = s.Model
	}
	if strings.HasPrefix(model, "<") { // a synthetic placeholder is not resumable
		model = ""
	}
	dir := o.dir
	if dir == "" {
		dir = s.Dir
	}

	hargs := strings.TrimSpace(h.Args + " " + strings.Join(o.hflags, " "))
	// A watched resume hangs on per-tool permission prompts the same way a launch
	// does; inject the harness's bypass flag (only claude needs one).
	if bp := autonomyBypass(h); bp != "" && !strings.Contains(" "+hargs+" ", " "+bp+" ") {
		hargs = strings.TrimSpace(hargs + " " + bp)
	}
	cmd := fillTemplate(tmpl, map[string]string{
		"id": s.ID, "dir": dir, "model": model, "task": o.task, "args": hargs,
	})

	// Same auth/env policy as a launch: default subscription OAuth (strip the API
	// key) unless --api/--auth/--env opt otherwise, applied as one shell prefix so
	// it holds on every dispatch path.
	depth := envInt("AX_DEPTH", -1) + 1
	maxDepth := firstNonZero(o.fen.maxDepth, envInt("AX_MAX_DEPTH", 1))
	env := axEnv(id, group, parent, depth, maxDepth, o.fen, m.Labels)
	if o.unattend {
		env = append(env, "AX_UNATTENDED=1")
	}
	cmd = applyEnvPolicy(cmd, o, h.Format, env)
	if mode == "interactive" {
		pretrustDir(h.Format, dir)
	}

	// Record the new task and mode on the same sidecar (same identity), and reopen
	// a concluded run so the continued session (if it is the root) concludes fresh
	// rather than reading the prior run's outcome.
	meta.Update(id, func(md *meta.Meta) {
		md.Task = o.task
		md.Mode = mode
		md.Outcome = ""
		md.FailReason = ""
		md.Exit = nil
		if o.closeOnDone && mode == "interactive" {
			md.CloseOnDone = true
		}
	})
	if parent == "" && group != "" && runs.Exists(group) {
		runs.Remove(group)
	}

	// Print identity first so a script can capture it, then dispatch exactly like a
	// launch: block on a job for --wait, open a tracked window under tmux, else run
	// detached. adoptAs is empty: a resume always addresses the known id, even for a
	// harness that mints its own on a fresh launch.
	a.printLaunched(id, group, o.jsonOut)
	switch {
	case o.wait:
		a.runWait(id, cmd, env, group, "", o)
	case a.mux.Active():
		title := launchWindowTitle(o.name, s.Harness, group)
		target := muxTargetFor(m.Labels, cfg.MuxGroup)
		a.mux.Open(dir, title, axEnvPrefix(env)+heldWindowCmd(id, cmd), id, target, origin == "human" && !o.unattend)
	default:
		a.runDetached(id, cmd, env, dir, "")
	}
}

// continueLiveReuse assigns a new task to a live, reuse-ready worker (a concluded
// keep-live worker between turns): the live counterpart to the resume-with-input
// path. Unlike raw `ax send` it reopens the task lifecycle before delivery, so
// `ax wait`/`ax result` track the NEW task rather than the stale success. It
// rewrites the task and clears the terminal/result markers, then delivers the
// task through the same backend `ax send` uses (the resident harness is already
// running; there is nothing to relaunch). Model/dir/auth flags do not apply here:
// they rebind a launch, and this reuses a live process as-is. On a delivery
// failure the prior done marker is restored, so the worker is never left reopened
// but untasked (which would strand a subsequent `ax wait`).
func (a App) continueLiveReuse(id, group string, o launchOpts) error {
	// Reopen the lifecycle: the new task, the previous outcome/result/exit cleared.
	// KeepLive/KeepUntil are left untouched so the worker stays warm for the next
	// reuse. This deliberately does NOT refresh KeepUntil: the lease is an absolute
	// wall-clock budget set at launch, not a sliding window renewed on each reuse, so
	// a busy worker still retires at its deadline rather than living forever.
	meta.Update(id, func(m *meta.Meta) {
		m.Task = o.task
		m.Mode = "interactive"
		m.Outcome = ""
		m.FailReason = ""
		m.Result = ""
		m.Exit = nil
	})
	// Clear the terminal hook marker so state.Terminal(id) is false again: an
	// `ax wait` on the new task must block until it concludes, not return
	// immediately on the stale "done". The harness's own hooks/watchers re-mark it
	// working on the new turn and done when it concludes.
	//
	// This meta.Update -> RemoveHook -> Send sequence is NOT atomic: for the sub-ms
	// window between the reopen and a successful Send there is no done marker, so a
	// concurrent reuse_ready poll or a lease-boundary reap racing this exact instant
	// could observe the worker as neither reuse-ready nor cleanly terminal. This is
	// acceptable under the single-supervisor model (the coordinator is the sole
	// reuser, so no second accept races this one), and a delivery failure below
	// restores the marker. A lock would only relocate the window, not remove it.
	state.RemoveHook(id)

	if err := a.mux.Send(id, o.task, true); err != nil {
		// Delivery failed: restore the done marker so the worker reads as concluded
		// (reuse-ready) again, not as a reopened worker that never received the task.
		// Emit NO success line/JSON in this case: printLaunched runs only after Send
		// succeeds, so stdout never claims success while the caller exits non-zero.
		state.WriteHook(id, "done")
		return fmt.Errorf("continue %s: deliver task to live worker: %w", id, err)
	}
	// Delivery succeeded: now it is safe to print identity to stdout. Emitting it
	// before Send would put {"id":...} on stdout even when Send fails and the
	// process exits 1, so automation parsing stdout would read success while the
	// exit code says failure.
	a.printLaunched(id, group, o.jsonOut)
	return nil
}
