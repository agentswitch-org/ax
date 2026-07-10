package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/notify"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
)

// HookState records a harness's own lifecycle-hook state (`ax hookstate
// <state>`), invoked by a hook installed via `ax hook install`. It reads the
// harness's hook JSON on stdin for the session id, falling back to AX_SESSION_ID.
// A hook-reported state is authoritative over ax's output/transcript inference.
func (a App) HookState(args []string) {
	if len(args) == 0 {
		return
	}
	var payload struct {
		SessionID string `json:"session_id"`
	}
	json.NewDecoder(os.Stdin).Decode(&payload) // best-effort; empty stdin is fine
	id := payload.SessionID
	if id == "" {
		id = os.Getenv("AX_SESSION_ID")
	}
	if id == "" {
		return
	}
	switch args[0] {
	case "stop":
		// The main agent's turn ended (claude's Stop hook). A task-carrying
		// interactive worker concludes here: with no human to steer it, the first
		// turn-end IS its task completing. A taskless session a human is driving
		// (or an explicit-idle report) keeps the plain idle behavior.
		a.concludeOrIdle(id)
	case "blocked":
		// Edge-trigger a notification when the harness's own hook reports a new
		// block (claude's Notification -> a permission/input prompt): a needs-you
		// the launching session did not choose. Fire only on the transition into blocked,
		// so a harness re-reporting blocked does not re-notify. Read prior first.
		prior, _ := state.HookState(id)
		state.WriteHook(id, "blocked")
		if prior != "blocked" {
			m := meta.Load(id)
			cfg, _ := config.Load()
			notify.Fire(cfg.Notify, notify.Event{
				ID: id, State: notify.NeedsYou, Summary: m.Task, Name: m.Name, Group: m.Group,
			})
		}
	default:
		state.WriteHook(id, args[0])
	}
}

// concludeOrIdle handles the main-agent turn-end reported by a harness's own
// lifecycle hook (claude's Stop). It concludes through the shared worker
// conclude path with the DEFERRED close (see closeSession): this process runs
// inside the harness's Stop hook, so tearing the session down synchronously
// would kill the harness while the hook is still in flight.
func (a App) concludeOrIdle(id string) {
	a.concludeWorker(id, closeSessionFn)
}

// closeSessionFn indirects the Stop-hook teardown so tests can stub it:
// closeSession execs this binary (`ax await-close`), and under `go test` that
// binary is the test runner itself.
var closeSessionFn = closeSession

// concludeWorker is the turn-end conclude choke point. A task-carrying watched
// worker concludes: it fires the done-review notify once (edge-triggered) and
// transitions into the visible done state, so the picker shows "done" instead of
// a frozen "idle". With --close-on-done it ends the session via closer instead
// of halting in the done state (the transcript survives for `ax read`). A
// taskless interactive session (no task, a human at the wheel) just goes idle,
// its unchanged behavior. closer is how the caller tears a --close-on-done
// session down: the deferred await-close spawn from inside a harness Stop hook
// (claude), or a direct live.Kill from the run wrapper's own transcript watcher
// (pi, codex), which has no in-flight hook to protect and whose own process IS
// the kill target's wrapper (a deferred wait-for-parent there would deadlock).
func (a App) concludeWorker(id string, closer func(string)) {
	m := meta.Load(id)
	if !concludable(m) {
		state.WriteHook(id, state.Idle)
		return
	}
	prior, _ := state.HookState(id)
	state.WriteHook(id, "done")
	if prior != "done" { // don't re-alert a worker that already concluded this turn
		cfg, _ := config.Load()
		notify.Fire(cfg.Notify, notify.Event{
			ID: id, State: notify.DoneReview, Summary: m.Task, Name: m.Name, Group: m.Group,
		})
	}
	// Snapshot the final report + exit into the durable record before any
	// teardown, so an interactive subscription-auth worker exposes the same
	// machine-readable final output a headless run does. A clean interactive
	// conclusion has no process exit code, so record 0.
	CaptureResult(id, 0)
	if m.CloseOnDone {
		// Clear only the pending-question sidecar; unlike killCleanup (the explicit
		// `ax kill` path) this must not remove the hook state, since the "done"
		// marker just written above is the whole point: it needs to survive
		// teardown so the picker reads a concluded worker, not a corpse.
		ask.Remove(id)
		// Use Update, not Save on the stale local m, so this does not clobber the
		// Result/Exit CaptureResult just wrote.
		meta.Update(id, func(m *meta.Meta) {
			if m.Outcome == "" {
				m.Outcome = "success"
			}
		})
		closer(id)
		return
	}
	cfg, _ := config.Load()
	maybeScheduleWorkerReap(id, m, cfg.Retention)
}

// ConcludeTurnEnd concludes a task-carrying interactive worker whose harness
// has no lifecycle hook (pi, codex): the run wrapper's transcript watcher calls
// it when the transcript shows the agent's turn ended (see session.TurnEnd),
// giving those harnesses the same authoritative done-gate claude's Stop hook
// provides. errReason non-empty means the turn ended in an error state (a model
// error, an aborted turn), which concludes failed instead of done. The
// --close-on-done teardown is a direct live.Kill: there is no in-flight hook to
// protect, and the caller is the wrapper's own process, so the deferred
// wait-for-parent close would wait on itself.
func ConcludeTurnEnd(id, errReason string) {
	m := meta.Load(id)
	if !concludable(m) {
		return
	}
	if errReason != "" {
		if state.Terminal(id) {
			return
		}
		m.Outcome = "failure"
		if m.FailReason == "" {
			m.FailReason = errReason
		}
		meta.Save(id, m)
		state.WriteHook(id, "failed")
		CaptureResult(id, 1)
		if m.CloseOnDone {
			live.Kill(id)
			return
		}
		cfg, _ := config.Load()
		maybeScheduleWorkerReap(id, m, cfg.Retention)
		return
	}
	App{}.concludeWorker(id, func(cid string) { live.Kill(cid) })
}

// CaptureResult snapshots a concluding session's final report (its last
// assistant message) and exit code into the durable meta record, so `ax result`
// can print them without scraping the pane. Best-effort and idempotent: it reads
// the transcript through session.Index (the same source `ax read` uses) and
// layers Result/Exit onto the record via meta.Update, so it never clobbers an
// Outcome a conclude path set alongside it. A session with no readable transcript
// still records the exit code. Called from every terminal path: the interactive
// done-gates (concludeWorker, ConcludeTurnEnd) and the wrapper's process exit
// (ConcludeExit).
func CaptureResult(id string, exit int) {
	if id == "" {
		return
	}
	cfg, _ := config.Load()
	report := finalReport(cfg, id)
	meta.Update(id, func(m *meta.Meta) {
		if report != "" {
			m.Result = report
		}
		e := exit
		m.Exit = &e
	})
}

// finalReport resolves a local session's transcript and returns its last
// assistant message, or "" when the session is unknown or has no assistant text.
func finalReport(cfg config.Config, id string) string {
	fmtOf := harnessFormats(cfg)
	for _, s := range session.Index(cfg) {
		if s.ID == id && s.Host == "" {
			return readFinalReport(fmtOf[s.Harness], s.File)
		}
	}
	return ""
}

// resultRetries and resultRetryWait bound how long readFinalReport waits for a
// streaming-partial transcript to receive the harness re-log. Five attempts at
// 50 ms each: 250 ms total, well under awaitCloseGrace (500 ms).
const (
	resultRetries   = 5
	resultRetryWait = 50 * time.Millisecond
)

// readFinalReport reads the last assistant message from a transcript file,
// retrying if the last turn is a streaming partial (output_tokens == 0).
// Claude writes a partial record during streaming and re-logs the complete
// message with usage tokens after the turn ends; a fast Stop-hook conclude
// races this re-log, capturing only the preamble. The retry waits for the
// re-log to land. After all retries, whatever text is present is returned.
func readFinalReport(format, path string) string {
	var text string
	for i := 0; i < resultRetries; i++ {
		if i > 0 {
			time.Sleep(resultRetryWait)
		}
		var complete bool
		text, complete = session.LastReportFull(format, path)
		if text == "" || complete {
			return text
		}
	}
	return text
}

// closeSession ends a --close-on-done session after its Stop hook has already
// recorded the done state, result, and outcome above. This function runs inside
// that same Stop hook invocation (`ax hookstate stop`, a child process the
// harness spawned), so it must NOT signal teardown synchronously: live.Kill
// signals the ax-run wrapper, which SIGTERMs the harness's whole process group
// and escalates to SIGKILL after a short grace, and the harness is at that
// moment still mid-Stop-hook, waiting for this very process to return. Killing
// it there force-ends a successful worker mid-hook (the window shows "running
// stop hook" and the run exits -1). The earlier fix (5feb41e) detached only
// this hook process from the group, which saved the hook's own writes but
// still killed the harness inside its hook window every time.
//
// Instead the teardown is deferred to a detached closer process (`ax
// await-close`, own session via Setsid so no group signal can reach it): it
// waits for THIS process to exit, which is the harness's own signal that the
// Stop hook completed, gives the harness a beat to settle back to idle, and
// only then triggers the kill. The harness then receives SIGTERM between
// turns, not mid-hook, and exits cleanly. If the closer cannot be spawned it
// falls back to the old direct kill (detached from the group first) so a
// --close-on-done session never lingers forever.
func closeSession(id string) {
	c := exec.Command(self(), "await-close", id, strconv.Itoa(os.Getpid()))
	setDetached(c)
	if err := startAndReap(c); err != nil {
		detachSelf()
		live.Kill(id)
	}
}

var workerReaperFn = spawnWorkerReaper
var workerReapKillFn = live.Kill

func maybeScheduleWorkerReap(id string, m meta.Meta, ret config.Retention) bool {
	if !reapEligible(m, ret) {
		return false
	}
	if m.KeepLive {
		// Indefinite keep-live (no lease): never schedule; the coordinator owns
		// teardown, and the worker stays warm until it explicitly ends it.
		if m.KeepUntil.IsZero() {
			return false
		}
		// A --keep-live-for lease: schedule the reap for lease expiry so the worker
		// is reaped when the lease elapses even if the coordinator crashed. The
		// reaper re-checks state and the lease at fire time. A lease already expired
		// by conclude time falls through to the normal delay.
		if now := time.Now(); now.Before(m.KeepUntil) {
			workerReaperFn(id, time.Until(m.KeepUntil))
			return true
		}
	}
	workerReaperFn(id, ret.ReapDelay())
	return true
}

// reapEligible is the reapable worker class, independent of keep-live: a
// parented, task-carrying interactive worker under a reap-enabled config. The
// keep-live exemption is layered on separately (keepLiveActive) because a
// --keep-live-for lease can elapse and make an exempt worker reapable again.
func reapEligible(m meta.Meta, ret config.Retention) bool {
	return ret.ReapConcludedWorkers &&
		m.Mode == "interactive" &&
		strings.TrimSpace(m.Task) != "" &&
		(m.Parent != "" || legacyUnparentedTrackedWorker(m))
}

func legacyUnparentedTrackedWorker(m meta.Meta) bool {
	if m.Parent != "" || m.Group == "" {
		return false
	}
	switch session.LabelValue(m.Labels, "role") {
	case "worker", "reviewer":
		return true
	default:
		return false
	}
}

// shouldReapWorker reports whether a worker is reapable right now: in the
// reapable class and not currently exempt by an in-force keep-live (an
// indefinite --keep-live, or a --keep-live-for lease that has not yet elapsed).
func shouldReapWorker(m meta.Meta, ret config.Retention) bool {
	return reapEligible(m, ret) && !keepLiveActive(m.KeepLive, m.KeepUntil, time.Now())
}

func spawnWorkerReaper(id string, delay time.Duration) {
	c := exec.Command(self(), "reap-worker", id, delay.String())
	setDetached(c)
	if err := startAndReap(c); err != nil {
		axlog.Printf("reap %s: schedule: %v", id, err)
	}
}

// ReapWorker is the detached delayed closer for ordinary parented interactive
// workers that reached done/failed but did not opt into --keep-live. It closes
// any mux viewer window, then kills the live process via live.Kill; transcripts,
// metadata, and terminal hook markers are left intact for read/result/wait/
// auto-retire.
func (a App) ReapWorker(args []string) {
	if len(args) < 2 {
		return
	}
	delay, err := time.ParseDuration(args[1])
	if err != nil || delay < 0 {
		delay = 60 * time.Second
	}
	id := args[0]
	time.Sleep(delay)
	if shouldReapWorkerNow(id) {
		a.closeReapedWorkerWindow(id)
		if err := workerReapKillFn(id); err != nil {
			axlog.Printf("reap %s: kill: %v", id, err)
		}
		return
	}
	cfg, _ := config.Load()
	w, ok := adoptedWorkerOrphanSafe(id, cfg, cfg.Retention)
	if !ok {
		return
	}
	a.closeReapedWorkerWindow(id)
	if err := killAdoptedWrapperFn(w); err != nil {
		axlog.Printf("reap %s: kill: %v", id, err)
	}
}

func (a App) closeReapedWorkerWindow(id string) {
	if !muxHasWindows(a.mux) {
		return
	}
	if err := a.mux.CloseWindow(id); err != nil {
		axlog.Printf("reap %s: close window: %v", id, err)
	}
}

func shouldReapWorkerNow(id string) bool {
	cfg, _ := config.Load()
	sessions := session.Index(cfg)
	rt := state.ComputeAll(sessions)
	r, ok := rt[id]
	if !ok {
		return false
	}
	if r.State != state.Live {
		return false
	}
	if reopenHooklessReapCandidate(id, cfg, sessions) {
		return false
	}
	pending := false
	if p, ok := ask.Load(id); ok && !p.Answered {
		pending = true
	}
	return workerReapSafeNow(id, meta.Load(id), cfg.Retention, r, pending)
}

// reopenHooklessReapCandidate is the delayed reaper's last chance to notice
// that a pi/codex worker was resumed in the same transcript after an older
// done/failed marker. Without this guard, a stale terminal marker can make the
// reaper SIGTERM the live resumed wrapper mid-turn.
func reopenHooklessReapCandidate(id string, cfg config.Config, sessions []session.Session) bool {
	if !hooklessTurnStartedAfterTerminal(id, cfg, sessions) {
		return false
	}
	ReopenTurnLifecycle(id)
	return true
}

func hooklessTurnStartedAfterTerminal(id string, cfg config.Config, sessions []session.Session) bool {
	terminalAt, ok := state.TerminalAt(id)
	if !ok {
		return false
	}
	m := meta.Load(id)
	if m.Mode != "interactive" || strings.TrimSpace(m.Task) == "" {
		return false
	}
	formats := harnessFormats(cfg)
	format := formats[m.Harness]
	for _, s := range sessions {
		if s.ID != id || s.Host != "" {
			continue
		}
		if format == "" {
			format = formats[s.Harness]
		}
		if format != "pi" && format != "codex" {
			return false
		}
		return session.TurnStartedAfter(format, s.File, terminalAt)
	}
	return false
}

// workerReapSafeNow is the shared safety gate for closing/killing a concluded
// worker now. The caller decides whether there is anything to reap (a live
// resident process, a tagged mux window, or both); this predicate only answers
// whether the session is in the same safe class as the delayed worker reaper.
func workerReapSafeNow(id string, m meta.Meta, ret config.Retention, r state.Runtime, pendingAsk bool) bool {
	if !shouldReapWorker(m, ret) {
		return false
	}
	if pendingAsk || state.WaitingOnChildren(id) {
		return false
	}
	return (r.Done || r.Failed) &&
		r.Activity != state.Working &&
		r.Waiting == ""
}

// awaitCloseTimeout bounds how long the detached closer waits for the Stop-hook
// process to exit before killing anyway, so a wedged harness that never lets
// its hook return cannot keep a --close-on-done session alive forever.
const awaitCloseTimeout = 60 * time.Second

// awaitCloseGrace is the pause between the Stop-hook process exiting and the
// kill, so the harness finishes its own turn-end bookkeeping and is idle when
// SIGTERM arrives.
const awaitCloseGrace = 500 * time.Millisecond

// AwaitClose is the detached --close-on-done closer (`ax await-close <id>
// <hook-pid>`, spawned by closeSession, not a user command): it waits for the
// Stop-hook process to exit (the harness's signal that the hook completed),
// pauses a beat, then ends the session via live.Kill. Split from closeSession
// so the wait happens outside the harness's hook window entirely.
func (a App) AwaitClose(args []string) {
	if len(args) < 2 {
		return
	}
	id := args[0]
	hookPid, err := strconv.Atoi(args[1])
	if err != nil || hookPid <= 0 {
		return
	}
	deadline := time.Now().Add(awaitCloseTimeout)
	for time.Now().Before(deadline) {
		// Signal 0 probes liveness without delivering anything; ESRCH = exited.
		if !processAlive(hookPid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(awaitCloseGrace)
	live.Kill(id)
}

// concludable reports whether a launch should conclude when its task finishes: a
// task-carrying watched (interactive) worker. Only task-carrying launches
// conclude, so a taskless interactive session a human is driving stays alive.
func concludable(m meta.Meta) bool {
	return m.Mode == "interactive" && strings.TrimSpace(m.Task) != ""
}

// ConcludeExit is the run wrapper's universal exit-time conclusion: it
// guarantees that ANY task-carrying session ax launched reaches a durable
// terminal state (done or failed) by the time its process is gone, no matter
// which harness or mode it ran in and no matter how it died. This is what
// makes `ax wait` deterministic: the richer signals (claude's Stop hook, the
// pi/codex transcript watcher, a clean headless exit) conclude earlier with
// more detail, and this backstop catches everything they miss (a crash, a
// kill, a harness that quit before concluding), so a waiter can never hang
// forever on a process that no longer exists.
//
// Rules, in order:
//   - not ax-launched (no meta), or taskless (a human driving an interactive
//     session): no conclusion, unchanged behavior.
//   - already terminal (done or failed): idempotent no-op, so a concluded
//     --close-on-done worker's teardown exit never overwrites its conclusion.
//   - a clean, un-stopped headless or recipe exit (code 0): done, outcome
//     success. Headless is the Stop-hook equivalent; recipe is the script's own
//     successful process completion.
//   - everything else: failed, outcome failure. That covers a non-zero
//     headless exit (reason from tail, keeping a more specific MarkFailed
//     reason already recorded), an ax-initiated stop (kill, fence trip,
//     restart) that ended the session before it concluded, and an interactive
//     worker whose harness died without its turn-end ever firing.
//
// The final report + exit code are snapshotted into the durable record either
// way (defer runs last, layered via meta.Update so it never clobbers the
// outcome written here).
func ConcludeExit(id string, exitCode int, tail string, stopped bool) {
	if id == "" {
		return
	}
	m := meta.Load(id)
	if m.Mode == "" || strings.TrimSpace(m.Task) == "" {
		return
	}
	if state.Terminal(id) {
		if !refreshReopenedTurnAtExit(id) {
			return
		}
		m = meta.Load(id)
	}
	if (m.Mode == "headless" || m.Mode == "recipe") && !stopped && exitCode == 0 {
		defer CaptureResult(id, exitCode)
		m.Outcome = "success"
		meta.Save(id, m)
		state.WriteHook(id, "done")
		return
	}
	// A failure conclusion records a non-zero exit even when the harness's
	// mapped code is 0 (an ax-initiated SIGTERM reads as an intentional stop),
	// so a caller reading {outcome, exit} never sees the contradictory
	// "failure, exit 0".
	if exitCode == 0 {
		exitCode = 1
	}
	defer CaptureResult(id, exitCode)
	m.Outcome = "failure"
	// MarkFailed may have already recorded a known-fatal reason from the run's
	// early output; that is more specific than a generic exit tail, keep it.
	if !state.Failed(id) && m.FailReason == "" {
		switch {
		case stopped:
			m.FailReason = "stopped before concluding"
		default:
			m.FailReason = reasonFromTail(tail)
			if m.FailReason == "" {
				m.FailReason = fmt.Sprintf("exited with code %d before concluding", exitCode)
			}
		}
	}
	meta.Save(id, m)
	state.WriteHook(id, "failed")
}

// MarkKilled marks a task-carrying worker failed when ax tears it down before
// it concluded (`ax kill`, a cascade kill, the picker's kill): killed before
// done IS a failure, and without a terminal marker a waiter holding its id
// would block forever on a session that no longer exists. It runs from the
// kill choke points rather than only the wrapper's exit, because the wrapper
// may already be dead (a crashed session being cleaned up) and then no exit
// path would ever write the marker. Idempotent, and never regresses a session
// that already concluded (its done marker is the record of a task that
// finished; a later kill is just closing its window).
func MarkKilled(id string) {
	if id == "" {
		return
	}
	m := meta.Load(id)
	if m.Mode == "" || strings.TrimSpace(m.Task) == "" {
		return
	}
	if state.Terminal(id) {
		return
	}
	m.Outcome = "failure"
	if m.FailReason == "" {
		m.FailReason = "killed"
	}
	meta.Save(id, m)
	state.WriteHook(id, "failed")
}

// MarkFailed marks a headless session failed with a short reason, the run
// wrapper's early-detection counterpart to ConcludeExit: it fires the
// instant a known-fatal error pattern (see FatalReason) shows up in the
// harness's own output, before the process has necessarily exited. Without
// this an agent that launches a worker doomed to fail immediately (an
// unsupported model, a 400, an empty credit balance, a missing login) has no
// signal until the process eventually dies, and in the meantime reads it as
// working. A no-op for anything not launched headless, and for a session
// already terminal (done or failed), so it never regresses a later, more
// specific state, or fires twice for the same run.
func MarkFailed(id, reason string) {
	if id == "" {
		return
	}
	m := meta.Load(id)
	if m.Mode != "headless" {
		return
	}
	if state.Done(id) || state.Failed(id) {
		return
	}
	m.Outcome = "failure"
	m.FailReason = reason
	meta.Save(id, m)
	state.WriteHook(id, "failed")
}

// fatalPatterns are known-fatal error signatures that show up in a headless
// harness's own output when a run was doomed from the start: an unsupported
// model, a rejected API request, an exhausted credit balance, or a missing
// login/auth. Matching one of these means the run cannot succeed no matter how
// long it is left running.
var fatalPatterns = []struct {
	match  *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)model[^\n]{0,40}not (found|supported)|unsupported model|not a valid model`), "unsupported model"},
	{regexp.MustCompile(`(?i)invalid_request(_error)?|400 .*bad request`), "invalid request"},
	{regexp.MustCompile(`(?i)credit balance (is )?too low|insufficient credit`), "credit balance too low"},
	{regexp.MustCompile(`(?i)(login|authentication) required|not logged in|please (log|sign) in|invalid api key|unauthorized`), "authentication required"},
}

// FatalReason scans a headless harness's own output for a known-fatal error
// signature (see fatalPatterns) and returns a short, stable reason for it. ok
// is false when nothing matched. Meant for the run wrapper's early output, not
// a full transcript: these signatures appear when a run is doomed from the
// start, so there is no need to keep scanning once a run is well underway.
func FatalReason(output string) (reason string, ok bool) {
	for _, p := range fatalPatterns {
		if p.match.MatchString(output) {
			return p.reason, true
		}
	}
	return "", false
}

// reasonFromTail turns a headless run's captured last output into a short,
// single-line failure reason for the picker and `ax list --json`: the last
// non-blank line, capped so one giant stack trace cannot blow out the sidecar
// or the picker's status line.
func reasonFromTail(tail string) string {
	lines := strings.Split(strings.TrimSpace(tail), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 200 {
				l = l[:200]
			}
			return l
		}
	}
	return ""
}

// Hook installs a harness's state hook (`ax hook install <harness>`): it writes a
// lifecycle hook into the harness's own settings so the harness reports
// working/idle/blocked authoritatively, the more robust path than scraping output.
func (a App) Hook(args []string) {
	if len(args) < 2 || args[0] != "install" {
		fmt.Fprintln(os.Stderr, "usage: ax hook install <harness>")
		os.Exit(2)
	}
	switch args[1] {
	case "claude":
		if err := installClaudeHook(); err != nil {
			fmt.Fprintln(os.Stderr, "ax:", err)
			os.Exit(1)
		}
		fmt.Println("installed ax state hooks into ~/.claude/settings.json")
	default:
		fmt.Fprintf(os.Stderr, "ax: no hook for %q; its state falls back to output inference\n", args[1])
		os.Exit(2)
	}
}

// installClaudeHook merges ax state hooks into ~/.claude/settings.json,
// idempotently: UserPromptSubmit -> working, Stop -> stop (the turn-end that a
// task worker concludes on), SubagentStop -> idle, Notification -> blocked. Stop
// and SubagentStop map to different verbs so a subagent finishing never concludes
// a worker whose main agent is still going.
func installClaudeHook() error {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".claude", "settings.json")
	m := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		// Refuse to touch a file we can't parse: writing m over it would replace
		// the user's whole settings file with just the ax hooks.
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("%s is not valid JSON (%v); fix it first, nothing was changed", path, err)
		}
	}
	hooks, _ := m["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	events := map[string]string{
		"UserPromptSubmit": "working",
		"Stop":             "stop",
		"SubagentStop":     "idle",
		"Notification":     "blocked",
	}
	for ev, st := range events {
		cmd := "ax hookstate " + st
		// Drop any ax hookstate entry we installed before (possibly with an older
		// verb, e.g. a pre-conclude `ax hookstate idle` on Stop) and re-add the
		// current one. This keeps re-running install idempotent and self-healing
		// across upgrades, never accumulating stale or conflicting ax hooks.
		list := withoutAxHookstate(hooks[ev])
		entry := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}}
		hooks[ev] = append(list, entry)
	}
	m["hooks"] = hooks
	os.MkdirAll(filepath.Dir(path), 0o755)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// withoutAxHookstate returns an event's hook groups with every ax-installed
// hookstate entry removed (any group whose command is an `ax hookstate ...`),
// leaving the user's own hooks untouched. The caller re-adds the current ax verb,
// so an upgrade replaces a stale verb instead of stacking a second, conflicting one.
func withoutAxHookstate(v any) []any {
	list, _ := v.([]any)
	out := make([]any, 0, len(list))
	for _, g := range list {
		if groupIsAxHookstate(g) {
			continue
		}
		out = append(out, g)
	}
	return out
}

// groupIsAxHookstate reports whether a hook group is one ax installed: any of its
// commands invokes `ax hookstate`.
func groupIsAxHookstate(g any) bool {
	gm, _ := g.(map[string]any)
	hs, _ := gm["hooks"].([]any)
	for _, h := range hs {
		hm, _ := h.(map[string]any)
		if c, _ := hm["command"].(string); strings.Contains(c, "ax hookstate ") {
			return true
		}
	}
	return false
}
