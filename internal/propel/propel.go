// Package propel is the ax outer loop for an inline harness that runs one burst
// per turn and then stops (pi, codex). A cloud model sustains its own agent loop
// and re-invokes itself each turn; a small local model (pi + gemma-12B) ends its
// turn with stopReason "stop" and nothing re-invokes it. When such a session is
// launched --self-propel, the run wrapper's turn-end watcher hands each turn-end
// to a Propeller, which decides whether to re-inject a continue-prompt (so the
// session keeps grinding) or to stop.
//
// The whole point is a stop-state machine that never spins forever and never
// prods a session that is legitimately waiting: it stops on a done signal
// (a configured check, or a done sentinel when no check is configured), does NOT
// re-inject while the session is waiting on a human, parks without submitting
// while the session's own delegated workers are still running, tolerates a short
// streak of transient error turns, and gives up after a cap of consecutive
// no-progress turns or total automatic submissions. Progress never replenishes
// the total budget. The pump is generic: it knows nothing about how
// a session organizes its work (no roles, no particular files); anything
// workflow-specific arrives via --propel-prompt / --propel-until / --propel-watch.
// Everything a decision depends on is an injected closure (Deps), so the state
// machine is unit-tested off synthetic transcripts with no pty, git, or
// filesystem involved.
package propel

import (
	"fmt"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/state"
)

// DefaultPrompt is the continue-prompt injected into an idle self-propelled
// session when no --propel-prompt was given. It is deliberately generic: the
// pump carries no knowledge of any particular workflow's files or roles, so a
// launch that wants a richer nudge overrides it with --propel-prompt.
const DefaultPrompt = "Finish the current task with one targeted action, then verify it. Reuse the findings and work already available; do not repeat discovery, restate plans, or delegate another review without a specific unresolved question. If complete, output PROJECT-COMPLETE on its own line. If blocked or unable to identify a useful next action, output PROJECT-BLOCKED on its own line and explain what is needed."

// DefaultMaxIdle is how many consecutive no-progress turns a propelled session
// may run before the pump gives up and marks it needs-attention, rather than
// re-injecting forever. A no-progress turn is one that left the external
// progress fingerprint (workspace contents or a watched artifact) unchanged.
const DefaultMaxIdle = 3

// DefaultMaxAutoTurns limits ALL automatic submissions, including error retries,
// worker wakeups, and lost-submit retries. Progress never replenishes this budget.
const DefaultMaxAutoTurns = 6

// DefaultWatchdog is how long after an inject the pump waits for any transcript
// activity before deciding the submit was lost (the injected prompt never became
// a turn). Such a stall counts as an idle turn, so the cap still advances and
// `ax wait` never hangs forever on a swallowed keystroke.
const DefaultWatchdog = 5 * time.Minute

// DefaultMaxErrors is how many CONSECUTIVE error turn-ends the pump tolerates,
// re-injecting after each, before concluding the session failed. A clean turn
// resets the streak, so one transient harness error never kills a long run.
const DefaultMaxErrors = 3

// DefaultBackoff is the pause before re-injecting after a turn-end, so a
// session that is briefly between things is not hammered: it re-checks, sees
// nothing new, ends its turn, and the idle cap eventually stops it if it is
// genuinely stuck rather than briefly waiting.
const DefaultBackoff = 3 * time.Second

// submitDelay is the pause between delivering the prompt text and the CR that
// submits it, mirroring the send fix and the unix backends: a full-screen
// harness TUI (pi, codex) coalesces a text burst and an immediately-following CR
// into one paste event and does NOT submit, so the two must arrive as separate
// writes with a gap between them.
const submitDelay = 150 * time.Millisecond

// Config is a self-propelled launch's resolved pump policy.
type Config struct {
	Prompt            string        // the continue-prompt injected each idle turn
	Task              string        // original task, included in the default continuation brief
	DoneCmd           string        // shell cmd run in the workspace; exit 0 satisfies the check ("" disables)
	RequireCompletion bool          // --accept is a gate on declared completion, not a stand-alone done predicate
	MaxIdle           int           // consecutive no-progress turns before giving up
	MaxAutoTurns      int           // total automatic submissions; zero uses the finite default
	Backoff           time.Duration // delay before re-injecting
	Watch             string        // --propel-watch: optional file whose content defines progress ("" => workspace)
	Watchdog          time.Duration // stall window: an inject with no transcript activity for this long counts as an idle turn (<=0 disables)
	MaxErrors         int           // consecutive error turn-ends tolerated before concluding failed
}

// ConfigFromSpec resolves the pump policy from a persisted launch spec, applying
// the built-in defaults for any field left unset. A nil spec (or one with
// SelfPropel off) still returns a usable config; the caller gates on
// spec.SelfPropel before constructing a Propeller.
func ConfigFromSpec(sp *meta.Spec) Config {
	c := Config{
		Prompt: DefaultPrompt, MaxIdle: DefaultMaxIdle, Backoff: DefaultBackoff,
		Watchdog: DefaultWatchdog, MaxErrors: DefaultMaxErrors, MaxAutoTurns: DefaultMaxAutoTurns,
	}
	if sp == nil {
		return c
	}
	if strings.TrimSpace(sp.PropelPrompt) != "" {
		c.Prompt = sp.PropelPrompt
	}
	c.DoneCmd = sp.PropelDone
	if c.DoneCmd == "" {
		c.DoneCmd = sp.Accept
		c.RequireCompletion = c.DoneCmd != ""
	}
	c.Task = sp.Task
	c.Watch = sp.PropelWatch
	if sp.PropelMaxIdle > 0 {
		c.MaxIdle = sp.PropelMaxIdle
	}
	if sp.PropelMaxAutoTurns > 0 {
		c.MaxAutoTurns = sp.PropelMaxAutoTurns
	}
	if sp.PropelBackoff != "" {
		if d, err := time.ParseDuration(sp.PropelBackoff); err == nil && d >= 0 {
			c.Backoff = d
		}
	}
	return c
}

// Deps are the effects the state machine depends on, injected so it can be
// tested with fakes. In production the run wrapper wires these to the real pty
// writer, workspace progress fingerprint, done-check, transcript reader,
// human-wait probe, live-worker count, conclude path, and notify.
type Deps struct {
	Write         func([]byte)        // write bytes into the session's own pty (the injection transport)
	AutoTurns     int                 // submissions already used by this session, restored from metadata
	SaveAutoTurns func(int) error     // persist BEFORE submitting; failure stops the pump
	Fingerprint   func() string       // external progress signal; equal across two turns => no progress
	DoneCheck     func() bool         // run the --propel-until command; true => exit 0 => done
	CheckFeedback func() string       // bounded output of the most recent failed check
	FinalReport   func() string       // the turn's final assistant message (scanned for a done sentinel)
	NeedsHuman    func() bool         // the session is blocked on / waiting for a human (do not prod)
	LiveChildren  func() int          // the session's still-running delegated workers (nil => 0)
	Conclude      func(reason string) // stop path: conclude so `ax wait` returns; "" = success, non-empty = failed with that reason
	NotifyStuck   func()              // fire a needs-attention alert when the idle cap trips
	Sleep         func(time.Duration) // backoff / submit-delay sleeper (a fake in tests)
	Now           func() time.Time    // clock for the submit watchdog (a fake in tests)
	Log           func(format string, args ...any)
}

// Propeller is the per-session stop-state machine. It is driven one turn-end at
// a time by the run wrapper's transcript watcher (OnTurnEnd / OnTurnError), plus
// a timer hook (Tick) for the things a turn-end can't see: waking a session
// parked on live workers, and the submit watchdog. It carries the idle-progress
// counter, the error streak, and the terminal "stopped" flag across turns. Not
// safe for concurrent use: the watcher calls it from one goroutine.
type Propeller struct {
	cfg Config
	d   Deps

	idle        int       // consecutive no-progress turns so far
	lastFP      string    // the previous turn's progress fingerprint
	started     bool      // a first turn-end has been seen (so it is never counted as idle)
	stopped     bool      // a stop condition fired; no further re-injection
	errStreak   int       // consecutive error turn-ends (a clean turn resets it)
	parked      bool      // waiting on live workers; Tick wakes the session when they finish
	awaiting    bool      // an inject is outstanding with no turn-end (or transcript activity) yet
	lastMark    time.Time // when the outstanding inject happened / the transcript last moved
	autoTurns   int       // monotonically increasing; progress and worker turnover never reset it
	checkPassed bool      // most recent clean turn's configured acceptance check
}

// New builds a Propeller for a self-propelled session.
func New(cfg Config, d Deps) *Propeller {
	if cfg.MaxAutoTurns <= 0 {
		cfg.MaxAutoTurns = DefaultMaxAutoTurns
	}
	if cfg.MaxIdle <= 0 {
		cfg.MaxIdle = DefaultMaxIdle
	}
	if d.Sleep == nil {
		d.Sleep = time.Sleep
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = func(string, ...any) {}
	}
	return &Propeller{cfg: cfg, d: d, autoTurns: d.AutoTurns}
}

// Stopped reports whether a stop condition has already fired, so the caller can
// skip a concluded session's later turn-ends.
func (p *Propeller) Stopped() bool { return p.stopped }

// Action is what the pump decided, for logging and testing.
type Action int

const (
	ActionReinject    Action = iota // re-injected the continue-prompt; the loop continues
	ActionDone                      // a done signal fired; concluded (success) and stopped
	ActionCapped                    // an idle/submission cap or persistence failure stopped the pump
	ActionWaitHuman                 // the session is waiting on a human; left untouched
	ActionWaitWorkers               // the session's delegated workers are still running; parked, cap untouched
	ActionErrored                   // too many consecutive error turns; concluded (failed) and stopped
	ActionNoop                      // already stopped / nothing to do
	ActionBlocked                   // explicit blocker; notify and stop without another submission
)

// OnTurnEnd runs the stop-state machine for one CLEAN turn-end (the caller only
// invokes it when the harness's transcript shows the turn ended with no error;
// an error turn goes through OnTurnError instead). In order:
//
//  1. If already stopped, do nothing.
//  2. A configured check passes (exit 0), with a completion signal if required,
//     and no workers are live: conclude, stop.
//  3. With no check, PROJECT-COMPLETE concludes successfully. PROJECT-BLOCKED
//     concludes with a failure reason instead of requesting more work. A done
//     signal never abandons live workers.
//  4. The session is waiting on a human: do NOT prod and do NOT conclude.
//     Leave it; when the human replies, its next turn drives the watcher again.
//  5. The session has live delegated workers: waiting on them is not a stall.
//     Park without touching the idle cap; Tick wakes it when they finish.
//  6. Idle cap: if the external progress fingerprint has been unchanged for
//     MaxIdle consecutive turns, give up. Notify needs-attention, conclude
//     FAILED, stop.
//  7. Reserve one submission from the finite total budget, or stop if exhausted.
//     Re-inject a targeted continuation brief to start the next turn.
func (p *Propeller) OnTurnEnd() Action {
	if p.stopped {
		return ActionNoop
	}
	p.awaiting = false
	p.parked = false
	p.errStreak = 0

	report := p.d.FinalReport()
	p.checkPassed = p.cfg.DoneCmd != "" && p.d.DoneCheck()
	declaredDone := MatchesDoneSentinel(report)
	noWorkers := p.liveChildren() == 0
	if p.checkPassed && (!p.cfg.RequireCompletion || declaredDone) && noWorkers {
		p.d.Log("propel: stop (configured check passed and completion conditions met)")
		p.finish()
		return ActionDone
	}

	if p.cfg.DoneCmd == "" && declaredDone && noWorkers {
		p.d.Log("propel: stop (done sentinel in final message)")
		p.finish()
		return ActionDone
	}
	if matchesSentinel(report, "PROJECT-BLOCKED") {
		return p.stop("self-propel stopped: agent reported a blocker; inspect ax result before restarting", ActionBlocked)
	}

	if p.d.NeedsHuman() {
		p.d.Log("propel: waiting on a human; not re-injecting")
		return ActionWaitHuman
	}

	if n := p.liveChildren(); n > 0 {
		p.parked = true
		p.d.Log("propel: %d live workers; parked until they finish", n)
		return ActionWaitWorkers
	}

	fp := p.d.Fingerprint()
	if p.started && fp == p.lastFP {
		p.idle++
	} else {
		p.idle = 0
	}
	p.lastFP = fp
	p.started = true

	if p.idle >= p.cfg.MaxIdle {
		return p.capOut()
	}

	if p.cfg.Backoff > 0 {
		p.d.Sleep(p.cfg.Backoff)
	}
	p.d.Log("propel: re-injecting continue-prompt (idle streak %d/%d)", p.idle, p.cfg.MaxIdle)
	return p.reinject("")
}

// OnTurnError runs the pump for a turn that ended IN ERROR (the transcript shows
// a model or harness error). A transient error is tolerated: the pump re-injects
// the continue-prompt within the total submission budget. MaxErrors consecutive
// error turns conclude failed; a clean turn resets only the error streak.
func (p *Propeller) OnTurnError(reason string) Action {
	if p.stopped {
		return ActionNoop
	}
	if p.d.NeedsHuman() {
		p.awaiting = false
		return ActionWaitHuman
	}
	p.awaiting = false
	p.parked = false
	p.errStreak++
	if p.errStreak >= p.cfg.MaxErrors {
		p.d.Log("propel: stop (%d consecutive error turns: %s)", p.errStreak, reason)
		p.stopped = true
		if strings.TrimSpace(reason) == "" {
			reason = "error turn"
		}
		p.d.Conclude(reason)
		return ActionErrored
	}
	p.d.Log("propel: error turn (%s); retrying (%d/%d)", reason, p.errStreak, p.cfg.MaxErrors)
	if p.cfg.Backoff > 0 {
		p.d.Sleep(p.cfg.Backoff)
	}
	return p.reinject(reason)
}

// NoteActivity acknowledges a submit. Once a turn is producing output, a later
// quiet interval must not inject another prompt into that still-running turn.
// A launch's token/time fences bound the running turn separately.
func (p *Propeller) NoteActivity() {
	p.awaiting = false
}

// Tick is the pump's timer hook, called by the watcher on every poll. It covers
// the two things a turn-end can't:
//
//   - Rechecking acceptance and the continuation budget once parked workers
//     finish. Worker turnover alone is not workspace progress.
//   - The submit watchdog: if an inject produced no transcript activity within
//     Watchdog, the submit was lost (a swallowed keystroke, a wedged composer).
//     The stall counts as an idle turn, so the cap still advances instead of
//     `ax wait` hanging forever, and the inject is retried.
//
// Cheap when nothing is pending; there is no busy-spin beyond the watcher's own
// poll cadence.
func (p *Propeller) Tick() Action {
	if p.stopped {
		return ActionNoop
	}
	if p.d.NeedsHuman() {
		p.awaiting = false
		return ActionWaitHuman
	}
	if p.parked {
		if p.liveChildren() > 0 {
			return ActionNoop
		}
		p.parked = false
		p.d.Log("propel: workers finished; checking acceptance and continuation budget")
		return p.OnTurnEnd()
	}
	if !p.awaiting || p.cfg.Watchdog <= 0 || p.d.Now().Sub(p.lastMark) < p.cfg.Watchdog {
		return ActionNoop
	}
	p.awaiting = false
	p.idle++
	if p.idle >= p.cfg.MaxIdle {
		return p.capOut()
	}
	p.d.Log("propel: no turn within %s of inject; counting an idle turn and retrying (%d/%d)",
		p.cfg.Watchdog, p.idle, p.cfg.MaxIdle)
	return p.reinject("The previous automatic submission produced no transcript activity.")
}

// capOut is the give-up path: the session ran out of road without finishing, so
// it concludes FAILED (a non-empty reason makes `ax result` report failure) and
// a needs-attention alert pulls a human in.
func (p *Propeller) capOut() Action {
	return p.stop(fmt.Sprintf("self-propel stopped: no workspace progress after %d consecutive idle turns; inspect ax result before restarting", p.idle), ActionCapped)
}

func (p *Propeller) stop(reason string, action Action) Action {
	p.d.Log("propel: %s", reason)
	p.stopped = true
	p.d.Conclude(reason)
	p.d.NotifyStuck()
	return action
}

// Every automatic input path goes through this budget gate. Persisting first
// means a wrapper restart or a failed submit cannot give the same tokens back.
func (p *Propeller) reinject(evidence string) Action {
	if p.d.NeedsHuman() {
		p.awaiting = false
		return ActionWaitHuman
	}
	if p.autoTurns >= p.cfg.MaxAutoTurns {
		return p.stop(fmt.Sprintf("self-propel stopped: automatic continuation limit reached (%d/%d); inspect ax result before restarting", p.autoTurns, p.cfg.MaxAutoTurns), ActionCapped)
	}
	p.autoTurns++
	if p.d.SaveAutoTurns != nil {
		if err := p.d.SaveAutoTurns(p.autoTurns); err != nil {
			return p.stop("self-propel stopped: cannot persist continuation budget: "+err.Error(), ActionCapped)
		}
	}
	prompt := p.cfg.Prompt
	if prompt == DefaultPrompt {
		if p.cfg.Task != "" {
			prompt += "\nCurrent task: " + clipped(p.cfg.Task, 1600)
		}
		if p.cfg.DoneCmd != "" {
			if p.checkPassed {
				prompt += "\nLast required check passed: " + clipped(p.cfg.DoneCmd, 300) + ". Verify the requested outcome before declaring completion."
			} else {
				prompt += "\nRequired check has not passed: " + clipped(p.cfg.DoneCmd, 300)
			}
			if !p.checkPassed && p.d.CheckFeedback != nil {
				prompt += "\nLatest check output: " + clipped(p.d.CheckFeedback(), 1200)
			}
		}
		if evidence != "" {
			prompt += "\nLatest evidence: " + clipped(evidence, 600)
		}
		prompt += fmt.Sprintf("\nAutomatic continuation %d/%d. Stop with PROJECT-BLOCKED if another attempt needs new information.", p.autoTurns, p.cfg.MaxAutoTurns)
	}
	p.inject(prompt)
	return ActionReinject
}

func clipped(s string, limit int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return string(r)
}

// finish concludes the session cleanly (an empty reason is the success path)
// and marks the pump stopped.
func (p *Propeller) finish() {
	p.stopped = true
	p.d.Conclude("")
}

// inject writes the continue-prompt into the session's own pty, delivering the
// (bracketed-paste-wrapped, for multi-line) text as one write and the submitting
// CR as a separate write after submitDelay, exactly as the send fix and the unix
// backends do so a burst-coalescing TUI actually submits. It also arms the
// submit watchdog.
func (p *Propeller) inject(text string) {
	payload := text
	if strings.Contains(text, "\n") {
		payload = "\x1b[200~" + text + "\x1b[201~"
	}
	if payload != "" {
		p.d.Write([]byte(payload))
		p.d.Sleep(submitDelay)
	}
	p.d.Write([]byte("\r"))
	p.awaiting = true
	p.lastMark = p.d.Now()
}

// liveChildren is the nil-safe read of the live delegated-worker count.
func (p *Propeller) liveChildren() int {
	if p.d.LiveChildren == nil {
		return 0
	}
	return p.d.LiveChildren()
}

// doneSentinels are the standalone lines a propelled session emits to declare
// its task complete. A match must be the whole trimmed line so the session
// discussing the sentinel in prose ("I will print PROJECT-COMPLETE when done")
// does not falsely stop the loop.
// MatchesDoneSentinel reports whether the session's final message contains a
// standalone done-sentinel line.
func MatchesDoneSentinel(report string) bool {
	return matchesSentinel(report, "PROJECT-COMPLETE")
}

func matchesSentinel(report, sentinel string) bool {
	for _, line := range strings.Split(report, "\n") {
		if strings.TrimSpace(line) == sentinel {
			return true
		}
	}
	return false
}

// NeedsHuman reports whether a session is waiting on a person: its own hook
// declared it blocked, or it has an unanswered `ax ask` outstanding. The pump
// must never prod such a session (that is a legitimate wait, not a stall).
func NeedsHuman(id string) bool {
	if state.Blocked(id) {
		return true
	}
	if p, ok := ask.Load(id); ok && !p.Answered {
		return true
	}
	return false
}

// LiveChildren counts the OTHER sessions of a run that are still running (a
// fresh heartbeat and not yet concluded): delegated work in flight. The pump
// never idle-caps a session whose workers are live (waiting on them is not a
// stall). Worker-count changes never replenish the continuation budget.
func LiveChildren(group, self string) int {
	if group == "" {
		return 0
	}
	fresh := live.LiveIDs()
	n := 0
	for id, m := range meta.LoadAll() {
		if id == self || m.Group != group {
			continue
		}
		if fresh[id] && !state.Terminal(id) {
			n++
		}
	}
	return n
}
