package propel

import (
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/meta"
)

// harness builds a Propeller wired to controllable fakes and records what it
// did: the bytes injected into the pty and whether it concluded / notified.
type harness struct {
	p          *Propeller
	written    []byte
	concluded  bool
	reason     string // what Conclude was called with ("" = success, else fail reason)
	notified   bool
	fp         string // current progress fingerprint (bump it to signal progress)
	report     string // current final assistant message
	needsHuman bool
	doneCheck  bool
	children   int       // live delegated workers
	now        time.Time // fake clock driving the submit watchdog
}

func newHarness(cfg Config) *harness {
	h := &harness{fp: "fp-0", now: time.Unix(1000, 0)}
	h.p = New(cfg, Deps{
		Write:        func(b []byte) { h.written = append(h.written, b...) },
		Fingerprint:  func() string { return h.fp },
		DoneCheck:    func() bool { return h.doneCheck },
		FinalReport:  func() string { return h.report },
		NeedsHuman:   func() bool { return h.needsHuman },
		LiveChildren: func() int { return h.children },
		Conclude:     func(r string) { h.concluded, h.reason = true, r },
		NotifyStuck:  func() { h.notified = true },
		Sleep:        func(time.Duration) {}, // never actually sleep in tests
		Now:          func() time.Time { return h.now },
	})
	return h
}

func (h *harness) reset() { h.written = h.written[:0] }

// defaultCfg is a fast test config: a small idle cap and no backoff.
func defaultCfg() Config {
	return Config{Prompt: "CONTINUE", MaxIdle: 3, Backoff: 0}
}

// TestReinjectWhenWorkRemains: a clean turn-end with progress being made, no
// sentinel, and no human wait re-injects the continue-prompt (text then CR).
func TestReinjectWhenWorkRemains(t *testing.T) {
	h := newHarness(defaultCfg())
	if act := h.p.OnTurnEnd(); act != ActionReinject {
		t.Fatalf("first turn: got action %v, want ActionReinject", act)
	}
	if got := string(h.written); got != "CONTINUE\r" {
		t.Fatalf("injected %q, want %q", got, "CONTINUE\r")
	}
	if h.concluded {
		t.Fatal("must not conclude while work remains")
	}
	// A second turn that made progress (fingerprint changed) re-injects again and
	// keeps the idle streak at zero.
	h.reset()
	h.fp = "fp-1"
	if act := h.p.OnTurnEnd(); act != ActionReinject {
		t.Fatalf("second turn: got %v, want ActionReinject", act)
	}
	if h.p.idle != 0 {
		t.Fatalf("idle streak = %d after progress, want 0", h.p.idle)
	}
}

// TestMultilinePromptBracketedPaste: a multi-line continue-prompt is wrapped in
// bracketed paste and submitted with a separate CR, so a burst-coalescing TUI
// submits it instead of leaving it in the composer.
func TestMultilinePromptBracketedPaste(t *testing.T) {
	cfg := defaultCfg()
	cfg.Prompt = "line one\nline two"
	h := newHarness(cfg)
	h.p.OnTurnEnd()
	want := "\x1b[200~line one\nline two\x1b[201~\r"
	if got := string(h.written); got != want {
		t.Fatalf("injected %q, want %q", got, want)
	}
}

// TestDoneSentinelStops: a done sentinel on its own line in the final message
// concludes the session and does NOT re-inject.
func TestDoneSentinelStops(t *testing.T) {
	h := newHarness(defaultCfg())
	h.report = "all shipped.\nPROJECT-COMPLETE\n"
	act := h.p.OnTurnEnd()
	if act != ActionDone {
		t.Fatalf("got %v, want ActionDone", act)
	}
	if !h.concluded {
		t.Fatal("done sentinel must conclude the session")
	}
	if len(h.written) != 0 {
		t.Fatalf("done sentinel must not re-inject, wrote %q", h.written)
	}
	if !h.p.Stopped() {
		t.Fatal("propeller must be stopped after a done sentinel")
	}
	// A later turn-end after stopping is a no-op.
	if act := h.p.OnTurnEnd(); act != ActionNoop {
		t.Fatalf("post-stop turn: got %v, want ActionNoop", act)
	}
}

// TestSentinelInProseDoesNotStop: the sentinel only matches a standalone line,
// so a coordinator mentioning it in prose keeps grinding.
func TestSentinelInProseDoesNotStop(t *testing.T) {
	h := newHarness(defaultCfg())
	h.report = "I will print PROJECT-COMPLETE when the whole thing is done."
	if act := h.p.OnTurnEnd(); act != ActionReinject {
		t.Fatalf("got %v, want ActionReinject (prose mention must not stop)", act)
	}
	if h.concluded {
		t.Fatal("a prose mention of the sentinel must not conclude")
	}
}

// TestDoneCheckStops: a --propel-until command that exits 0 concludes the loop,
// ahead of any sentinel or idle logic.
func TestDoneCheckStops(t *testing.T) {
	cfg := defaultCfg()
	cfg.DoneCmd = "true"
	h := newHarness(cfg)
	h.doneCheck = true
	act := h.p.OnTurnEnd()
	if act != ActionDone {
		t.Fatalf("got %v, want ActionDone", act)
	}
	if !h.concluded || len(h.written) != 0 {
		t.Fatalf("propel-until pass must conclude and not inject (concluded=%v wrote=%q)", h.concluded, h.written)
	}
}

// TestNeedsHumanDoesNotProd: while the session is waiting on a human, the
// pump neither re-injects nor concludes; it leaves the session alone.
func TestNeedsHumanDoesNotProd(t *testing.T) {
	h := newHarness(defaultCfg())
	h.needsHuman = true
	act := h.p.OnTurnEnd()
	if act != ActionWaitHuman {
		t.Fatalf("got %v, want ActionWaitHuman", act)
	}
	if len(h.written) != 0 {
		t.Fatalf("must not prod a human-waiting session, wrote %q", h.written)
	}
	if h.concluded {
		t.Fatal("must not conclude a session that is waiting on a human")
	}
	if h.p.Stopped() {
		t.Fatal("waiting on a human is not a terminal stop")
	}
}

// TestMaxIdleTurnsCaps: after MaxIdle consecutive no-progress turns (fingerprint
// unchanged), the pump gives up: it notifies needs-attention, concludes, and
// stops re-injecting.
func TestMaxIdleTurnsCaps(t *testing.T) {
	h := newHarness(defaultCfg()) // MaxIdle = 3
	// Turn 1: first ever turn-end, never counted as idle -> re-inject.
	if act := h.p.OnTurnEnd(); act != ActionReinject {
		t.Fatalf("turn 1: got %v, want ActionReinject", act)
	}
	// Turns 2 and 3: no progress (fp unchanged) -> idle 1, 2 -> still re-inject.
	for i := 2; i <= 3; i++ {
		if act := h.p.OnTurnEnd(); act != ActionReinject {
			t.Fatalf("turn %d: got %v, want ActionReinject (idle=%d)", i, act, h.p.idle)
		}
	}
	// Turn 4: idle streak reaches MaxIdle=3 -> cap.
	act := h.p.OnTurnEnd()
	if act != ActionCapped {
		t.Fatalf("turn 4: got %v, want ActionCapped", act)
	}
	if !h.notified {
		t.Fatal("idle cap must fire a needs-attention notify")
	}
	if !h.concluded {
		t.Fatal("idle cap must conclude so `ax wait` returns")
	}
	if h.reason == "" {
		t.Fatal("idle cap must conclude FAILED (non-empty reason)")
	}
	if !h.p.Stopped() {
		t.Fatal("propeller must be stopped after the idle cap")
	}
}

// TestProgressResetsIdleStreak: a productive turn in the middle of an idle run
// resets the streak, so a coordinator that is slow but advancing never caps.
func TestProgressResetsIdleStreak(t *testing.T) {
	h := newHarness(Config{Prompt: "CONTINUE", MaxIdle: 3})
	h.p.OnTurnEnd() // turn 1 (started)
	h.p.OnTurnEnd() // idle 1
	if h.p.idle != 1 {
		t.Fatalf("idle = %d, want 1", h.p.idle)
	}
	h.fp = "moved" // progress
	h.p.OnTurnEnd()
	if h.p.idle != 0 {
		t.Fatalf("idle = %d after progress, want 0", h.p.idle)
	}
	// It would now take another full MaxIdle stalls to cap, proving no premature stop.
	if h.p.Stopped() {
		t.Fatal("must not be stopped after a productive turn")
	}
}

// TestConfigFromSpecDefaults: an absent spec, and a spec that opts in without
// tuning, both resolve to the built-in defaults; explicit fields override.
func TestConfigFromSpecDefaults(t *testing.T) {
	def := ConfigFromSpec(nil)
	if def.Prompt != DefaultPrompt || def.MaxIdle != DefaultMaxIdle || def.Backoff != DefaultBackoff ||
		def.DoneCmd != "" || def.Watch != "" || def.Watchdog != DefaultWatchdog || def.MaxErrors != DefaultMaxErrors {
		t.Fatalf("nil spec config = %+v, want built-in defaults", def)
	}
	got := ConfigFromSpec(&meta.Spec{
		SelfPropel: true, PropelPrompt: "GO", PropelDone: "./done.sh",
		PropelMaxIdle: 5, PropelBackoff: "1s", PropelWatch: "task.md",
	})
	if got.Prompt != "GO" || got.DoneCmd != "./done.sh" || got.MaxIdle != 5 ||
		got.Backoff != time.Second || got.Watch != "task.md" {
		t.Fatalf("explicit spec config = %+v, want overrides applied", got)
	}
}

// TestMatchesDoneSentinel pins the sentinel matcher: a standalone sentinel line
// (any of the accepted tokens) matches; a substring or prose mention does not.
func TestMatchesDoneSentinel(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"PROJECT-COMPLETE", true},
		{"  PROJECT-COMPLETE  ", true},
		{"work done\nPROJECT-COMPLETE", true},
		{"I will emit PROJECT-COMPLETE later", false},
		{"PROJECT-COMPLETE-NOT", false},
		{"nothing here", false},
		{"", false},
	}
	for _, c := range cases {
		if got := MatchesDoneSentinel(c.in); got != c.want {
			t.Errorf("MatchesDoneSentinel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestBackoffSleepsBeforeReinject: when a backoff is configured, the pump sleeps
// before injecting (so a coordinator briefly waiting on workers is not hammered).
func TestBackoffSleepsBeforeReinject(t *testing.T) {
	var slept time.Duration
	cfg := Config{Prompt: "CONTINUE", MaxIdle: 3, Backoff: 5 * time.Second}
	h := &harness{fp: "fp-0"}
	h.p = New(cfg, Deps{
		Write:       func(b []byte) { h.written = append(h.written, b...) },
		Fingerprint: func() string { return h.fp },
		DoneCheck:   func() bool { return false },
		FinalReport: func() string { return "" },
		NeedsHuman:  func() bool { return false },
		Conclude:    func(string) { h.concluded = true },
		NotifyStuck: func() {},
		Sleep:       func(d time.Duration) { slept += d },
	})
	h.p.OnTurnEnd()
	// The pump sleeps the backoff before injecting, then submitDelay between the
	// prompt text and the submitting CR.
	if want := 5*time.Second + submitDelay; slept != want {
		t.Fatalf("slept %s, want %s (backoff + submitDelay)", slept, want)
	}
}

// TestCapConcludesFailure: the idle cap is a give-up, not a success. Conclude
// receives a non-empty fail reason (so `ax result` reports failed), while the
// done paths (sentinel, --propel-until) conclude with an empty reason (success).
func TestCapConcludesFailure(t *testing.T) {
	h := newHarness(Config{Prompt: "GO", MaxIdle: 1})
	h.p.OnTurnEnd() // first turn arms the fingerprint
	if act := h.p.OnTurnEnd(); act != ActionCapped {
		t.Fatalf("got %v, want ActionCapped", act)
	}
	if !h.concluded || h.reason == "" {
		t.Fatalf("idle cap must conclude FAILED, got concluded=%v reason=%q", h.concluded, h.reason)
	}

	done := newHarness(defaultCfg())
	done.report = "PROJECT-COMPLETE"
	if act := done.p.OnTurnEnd(); act != ActionDone {
		t.Fatalf("got %v, want ActionDone", act)
	}
	if !done.concluded || done.reason != "" {
		t.Fatalf("done sentinel must conclude SUCCESS (empty reason), got reason=%q", done.reason)
	}

	until := newHarness(Config{Prompt: "GO", MaxIdle: 3, DoneCmd: "true"})
	until.doneCheck = true
	if act := until.p.OnTurnEnd(); act != ActionDone {
		t.Fatalf("got %v, want ActionDone", act)
	}
	if !until.concluded || until.reason != "" {
		t.Fatalf("--propel-until pass must conclude SUCCESS (empty reason), got reason=%q", until.reason)
	}
}

// TestNoCapWhileLiveWorkers: a session whose delegated workers are still running
// is waiting, not stalled: the pump neither counts idle turns nor concludes nor
// prods it, however long the wait, and Tick wakes it the moment they all finish.
func TestNoCapWhileLiveWorkers(t *testing.T) {
	h := newHarness(Config{Prompt: "GO", MaxIdle: 2})
	h.children = 3
	for i := 1; i <= 10; i++ { // far past MaxIdle
		if act := h.p.OnTurnEnd(); act != ActionWaitWorkers {
			t.Fatalf("turn %d: got %v, want ActionWaitWorkers", i, act)
		}
	}
	if h.concluded || h.p.Stopped() || h.p.idle != 0 {
		t.Fatalf("live workers must not advance the cap (concluded=%v stopped=%v idle=%d)",
			h.concluded, h.p.Stopped(), h.p.idle)
	}
	if len(h.written) != 0 {
		t.Fatalf("parked pump must not prod the session, wrote %q", h.written)
	}
	// While workers are still live, Tick leaves the parked session alone.
	if act := h.p.Tick(); act != ActionNoop {
		t.Fatalf("Tick with live workers: got %v, want ActionNoop", act)
	}
	// The workers finish: the next Tick wakes the session to collect results.
	h.children = 0
	if act := h.p.Tick(); act != ActionReinject {
		t.Fatalf("Tick after workers done: got %v, want ActionReinject", act)
	}
	if got := string(h.written); got != "GO\r" {
		t.Fatalf("wake injected %q, want %q", got, "GO\r")
	}
}

// TestWatchdogAdvancesCap: an inject that produces no transcript activity within
// the watchdog window is a lost submit. Each stall counts as an idle turn (and
// retries the inject), so the cap still advances and `ax wait` never hangs,
// while transcript activity restarts the window (a running turn is not a stall).
func TestWatchdogAdvancesCap(t *testing.T) {
	h := newHarness(Config{Prompt: "GO", MaxIdle: 2, Watchdog: time.Minute})
	h.p.OnTurnEnd() // injects; arms the watchdog
	h.reset()

	if act := h.p.Tick(); act != ActionNoop {
		t.Fatalf("Tick inside the window: got %v, want ActionNoop", act)
	}
	// Transcript activity restarts the window: the submit clearly landed.
	h.now = h.now.Add(50 * time.Second)
	h.p.NoteActivity()
	h.now = h.now.Add(50 * time.Second)
	if act := h.p.Tick(); act != ActionNoop {
		t.Fatalf("Tick after activity restarted the window: got %v, want ActionNoop", act)
	}
	// A full silent window: stall #1 counts as an idle turn and retries the inject.
	h.now = h.now.Add(time.Minute)
	if act := h.p.Tick(); act != ActionReinject {
		t.Fatalf("first stall: got %v, want ActionReinject", act)
	}
	if h.p.idle != 1 {
		t.Fatalf("idle = %d after a stall, want 1", h.p.idle)
	}
	if got := string(h.written); got != "GO\r" {
		t.Fatalf("stall retry injected %q, want %q", got, "GO\r")
	}
	// A second silent window reaches MaxIdle: cap, notified, concluded FAILED.
	h.now = h.now.Add(2 * time.Minute)
	if act := h.p.Tick(); act != ActionCapped {
		t.Fatalf("second stall: got %v, want ActionCapped", act)
	}
	if !h.concluded || h.reason == "" || !h.notified {
		t.Fatalf("watchdog cap must notify and conclude failed (concluded=%v reason=%q notified=%v)",
			h.concluded, h.reason, h.notified)
	}
	if !h.p.Stopped() {
		t.Fatal("propeller must be stopped after the watchdog cap")
	}
}

// TestTransientErrorRetryThenFail: an error turn-end under the pump is retried
// (re-injected) rather than immediately fatal; only MaxErrors CONSECUTIVE error
// turns conclude the session failed, and a clean turn resets the streak.
func TestTransientErrorRetryThenFail(t *testing.T) {
	h := newHarness(Config{Prompt: "GO", MaxIdle: 10, MaxErrors: 3})
	// Two consecutive errors: retried, not concluded.
	for i := 1; i <= 2; i++ {
		h.reset()
		if act := h.p.OnTurnError("model error"); act != ActionReinject {
			t.Fatalf("error %d: got %v, want ActionReinject", i, act)
		}
		if got := string(h.written); got != "GO\r" {
			t.Fatalf("error retry %d injected %q, want %q", i, got, "GO\r")
		}
	}
	// A clean turn resets the streak, so two MORE errors are still retried.
	h.fp = "moved"
	h.p.OnTurnEnd()
	for i := 1; i <= 2; i++ {
		if act := h.p.OnTurnError("model error"); act != ActionReinject {
			t.Fatalf("post-reset error %d: got %v, want ActionReinject", i, act)
		}
	}
	if h.concluded {
		t.Fatal("a streak below MaxErrors must not conclude")
	}
	// The third consecutive error trips the streak: conclude FAILED with the reason.
	if act := h.p.OnTurnError("model error"); act != ActionErrored {
		t.Fatalf("third consecutive error: got %v, want ActionErrored", act)
	}
	if !h.concluded || h.reason != "model error" {
		t.Fatalf("error streak must conclude failed with the reason, got concluded=%v reason=%q",
			h.concluded, h.reason)
	}
	if !h.p.Stopped() {
		t.Fatal("propeller must be stopped after the error streak")
	}
}

// TestPromptOverrideAndGenericDefault: --propel-prompt (via the spec) replaces
// the injected continue-prompt, and the built-in default is generic: the pump
// mechanism carries no knowledge of any particular workflow's files or roles.
func TestPromptOverrideAndGenericDefault(t *testing.T) {
	got := ConfigFromSpec(&meta.Spec{SelfPropel: true, PropelPrompt: "PUSH ON"})
	if got.Prompt != "PUSH ON" {
		t.Fatalf("--propel-prompt not honored: %q", got.Prompt)
	}
	h := newHarness(Config{Prompt: ConfigFromSpec(&meta.Spec{SelfPropel: true}).Prompt, MaxIdle: 3})
	h.p.OnTurnEnd()
	if gotW := string(h.written); gotW != DefaultPrompt+"\r" {
		t.Fatalf("default inject = %q, want DefaultPrompt", gotW)
	}
	for _, banned := range []string{"coordin" + "ator", "back" + "log"} {
		if strings.Contains(strings.ToLower(DefaultPrompt), banned) {
			t.Fatalf("DefaultPrompt must be generic; contains %q", banned)
		}
	}
}
