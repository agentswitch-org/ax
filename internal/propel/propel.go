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
// (a --propel-until check, or a done sentinel in the final message), does NOT
// re-inject while the session is waiting on a human, parks (without ever capping)
// while the session's own delegated workers are still running, tolerates a short
// streak of transient error turns, and gives up after a cap of consecutive
// no-progress turns. The pump is a generic mechanism: it knows nothing about how
// a session organizes its work (no roles, no particular files); anything
// workflow-specific arrives via --propel-prompt / --propel-until / --propel-watch.
// Everything a decision depends on is an injected closure (Deps), so the state
// machine is unit-tested off synthetic transcripts with no pty, git, or
// filesystem involved.
package propel

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
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
const DefaultPrompt = "Your previous turn ended but the task is not finished. Continue now: take the next concrete action. If the task is fully complete, output the line PROJECT-COMPLETE on its own line."

// DefaultMaxIdle is how many consecutive no-progress turns a propelled session
// may run before the pump gives up and marks it needs-attention, rather than
// re-injecting forever. A no-progress turn is one that left the external
// progress fingerprint (git state, run sessions, live workers) unchanged.
const DefaultMaxIdle = 8

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
	Prompt    string        // the continue-prompt injected each idle turn
	DoneCmd   string        // shell cmd run in the workspace; exit 0 => task complete ("" disables)
	MaxIdle   int           // consecutive no-progress turns before giving up
	Backoff   time.Duration // delay before re-injecting
	Watch     string        // --propel-watch: optional file whose mtime change counts as progress ("" => none)
	Watchdog  time.Duration // stall window: an inject with no transcript activity for this long counts as an idle turn (<=0 disables)
	MaxErrors int           // consecutive error turn-ends tolerated before concluding failed
}

// ConfigFromSpec resolves the pump policy from a persisted launch spec, applying
// the built-in defaults for any field left unset. A nil spec (or one with
// SelfPropel off) still returns a usable config; the caller gates on
// spec.SelfPropel before constructing a Propeller.
func ConfigFromSpec(sp *meta.Spec) Config {
	c := Config{
		Prompt: DefaultPrompt, MaxIdle: DefaultMaxIdle, Backoff: DefaultBackoff,
		Watchdog: DefaultWatchdog, MaxErrors: DefaultMaxErrors,
	}
	if sp == nil {
		return c
	}
	if strings.TrimSpace(sp.PropelPrompt) != "" {
		c.Prompt = sp.PropelPrompt
	}
	c.DoneCmd = sp.PropelDone
	c.Watch = sp.PropelWatch
	if sp.PropelMaxIdle > 0 {
		c.MaxIdle = sp.PropelMaxIdle
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
// writer, git/session progress fingerprint, done-check, transcript reader,
// human-wait probe, live-worker count, conclude path, and notify.
type Deps struct {
	Write        func([]byte)        // write bytes into the session's own pty (the injection transport)
	Fingerprint  func() string       // external progress signal; equal across two turns => no progress
	DoneCheck    func() bool         // run the --propel-until command; true => exit 0 => done
	FinalReport  func() string       // the turn's final assistant message (scanned for a done sentinel)
	NeedsHuman   func() bool         // the session is blocked on / waiting for a human (do not prod)
	LiveChildren func() int          // the session's still-running delegated workers (nil => 0)
	Conclude     func(reason string) // stop path: conclude so `ax wait` returns; "" = success, non-empty = failed with that reason
	NotifyStuck  func()              // fire a needs-attention alert when the idle cap trips
	Sleep        func(time.Duration) // backoff / submit-delay sleeper (a fake in tests)
	Now          func() time.Time    // clock for the submit watchdog (a fake in tests)
	Log          func(format string, args ...any)
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

	idle      int       // consecutive no-progress turns so far
	lastFP    string    // the previous turn's progress fingerprint
	started   bool      // a first turn-end has been seen (so it is never counted as idle)
	stopped   bool      // a stop condition fired; no further re-injection
	errStreak int       // consecutive error turn-ends (a clean turn resets it)
	parked    bool      // waiting on live workers; Tick wakes the session when they finish
	awaiting  bool      // an inject is outstanding with no turn-end (or transcript activity) yet
	lastMark  time.Time // when the outstanding inject happened / the transcript last moved
}

// New builds a Propeller for a self-propelled session.
func New(cfg Config, d Deps) *Propeller {
	if d.Sleep == nil {
		d.Sleep = time.Sleep
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = func(string, ...any) {}
	}
	return &Propeller{cfg: cfg, d: d}
}

// Stopped reports whether a stop condition has already fired, so the caller can
// skip a concluded session's later turn-ends.
func (p *Propeller) Stopped() bool { return p.stopped }

// Action is what the pump decided, for logging and testing.
type Action int

const (
	ActionReinject    Action = iota // re-injected the continue-prompt; the loop continues
	ActionDone                      // a done signal fired; concluded (success) and stopped
	ActionCapped                    // the idle cap tripped; notified + concluded (failed) and stopped
	ActionWaitHuman                 // the session is waiting on a human; left untouched
	ActionWaitWorkers               // the session's delegated workers are still running; parked, cap untouched
	ActionErrored                   // too many consecutive error turns; concluded (failed) and stopped
	ActionNoop                      // already stopped / nothing to do
)

// OnTurnEnd runs the stop-state machine for one CLEAN turn-end (the caller only
// invokes it when the harness's transcript shows the turn ended with no error;
// an error turn goes through OnTurnError instead). In order:
//
//  1. If already stopped, do nothing.
//  2. --propel-until check passes (exit 0): the task is done. Conclude, stop.
//  3. A done sentinel in the final assistant message (PROJECT-COMPLETE): the
//     session declared completion. Conclude, stop.
//  4. The session is waiting on a human: do NOT prod and do NOT conclude.
//     Leave it; when the human replies, its next turn drives the watcher again.
//  5. The session has live delegated workers: waiting on them is not a stall.
//     Park without touching the idle cap; Tick wakes it when they finish.
//  6. Idle cap: if the external progress fingerprint has been unchanged for
//     MaxIdle consecutive turns, give up. Notify needs-attention, conclude
//     FAILED, stop.
//  7. Otherwise there is work to do and progress is being made: after a short
//     backoff, re-inject the continue-prompt to start the next turn.
func (p *Propeller) OnTurnEnd() Action {
	if p.stopped {
		return ActionNoop
	}
	p.awaiting = false
	p.parked = false
	p.errStreak = 0

	if p.cfg.DoneCmd != "" && p.d.DoneCheck() {
		p.d.Log("propel: stop (--propel-until check passed)")
		p.finish()
		return ActionDone
	}

	if MatchesDoneSentinel(p.d.FinalReport()) {
		p.d.Log("propel: stop (done sentinel in final message)")
		p.finish()
		return ActionDone
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
	p.inject(p.cfg.Prompt)
	return ActionReinject
}

// OnTurnError runs the pump for a turn that ended IN ERROR (the transcript shows
// a model or harness error). A transient error is tolerated: the pump re-injects
// the continue-prompt, and only MaxErrors CONSECUTIVE error turns conclude the
// session failed. Any clean turn resets the streak.
func (p *Propeller) OnTurnError(reason string) Action {
	if p.stopped {
		return ActionNoop
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
	p.inject(p.cfg.Prompt)
	return ActionReinject
}

// NoteActivity tells the pump the session's transcript just moved: an
// outstanding inject is being worked on, so the watchdog window restarts.
func (p *Propeller) NoteActivity() {
	if p.awaiting {
		p.lastMark = p.d.Now()
	}
}

// Tick is the pump's timer hook, called by the watcher on every poll. It covers
// the two things a turn-end can't:
//
//   - Waking a session that was parked on live workers once they all finish
//     (their completion is exactly the progress it was waiting for), so nothing
//     hangs when the workers report while the session sits idle.
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
	if p.parked {
		if p.liveChildren() > 0 {
			return ActionNoop
		}
		p.parked = false
		p.idle = 0 // worker completion is progress
		p.d.Log("propel: workers finished; re-injecting to collect their results")
		p.inject(p.cfg.Prompt)
		return ActionReinject
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
	p.inject(p.cfg.Prompt)
	return ActionReinject
}

// capOut is the give-up path: the session ran out of road without finishing, so
// it concludes FAILED (a non-empty reason makes `ax result` report failure) and
// a needs-attention alert pulls a human in.
func (p *Propeller) capOut() Action {
	p.d.Log("propel: stop (no progress after %d idle turns)", p.idle)
	p.stopped = true
	p.d.NotifyStuck()
	p.d.Conclude(fmt.Sprintf("self-propel gave up: no progress after %d consecutive idle turns", p.idle))
	return ActionCapped
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
var doneSentinels = []string{"PROJECT-COMPLETE"}

// MatchesDoneSentinel reports whether the session's final message contains a
// standalone done-sentinel line.
func MatchesDoneSentinel(report string) bool {
	for _, line := range strings.Split(report, "\n") {
		t := strings.TrimSpace(line)
		for _, s := range doneSentinels {
			if t == s {
				return true
			}
		}
	}
	return false
}

// RunDoneCheck runs the --propel-until shell command in dir and reports whether
// it exited 0 (the authoritative "task done" signal). Output is discarded;
// only the exit status matters.
func RunDoneCheck(cmd, dir string) bool {
	if strings.TrimSpace(cmd) == "" {
		return false
	}
	c := exec.Command("sh", "-c", cmd)
	c.Dir = dir
	return c.Run() == nil
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
// stall), and a change in this count is progress.
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

// Fingerprint is the default external progress signal: a hash of the session
// workspace's git state (HEAD + uncommitted tree), the number of sessions in
// the run, the number of those still live, and (when --propel-watch names one)
// a watched file's mtime. Any real action (a commit, an edit, a spawned or
// finished worker, a watched-file update) changes at least one of these, so an
// unchanged fingerprint across turns is a genuine no-progress signal that feeds
// the idle cap. A workspace with none of these signals hashes to a constant, so
// a truly-inert session still caps out safely. Deliberately generic: no
// workflow-specific paths are baked in; a launch that tracks progress through a
// particular file points --propel-watch at it.
func Fingerprint(dir, group, self, watch string) string {
	var b strings.Builder
	if head, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output(); err == nil {
		b.Write(head)
	}
	if status, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); err == nil {
		b.Write(status)
	}
	b.WriteString("|" + strconv.Itoa(groupSessionCount(group)))
	b.WriteString("|" + strconv.Itoa(LiveChildren(group, self)))
	if watch != "" {
		if fi, err := os.Stat(watch); err == nil {
			b.WriteString("|" + fi.ModTime().UTC().String())
		}
	}
	sum := sha1.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// groupSessionCount counts the meta sidecars belonging to a run, so a newly
// spawned worker (which writes a sidecar at launch) registers as progress.
func groupSessionCount(group string) int {
	if group == "" {
		return 0
	}
	n := 0
	for _, m := range meta.LoadAll() {
		if m.Group == group {
			n++
		}
	}
	return n
}
