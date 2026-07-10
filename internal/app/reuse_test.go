package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
	"github.com/agentswitch-org/ax/internal/wire"
)

// recordMux captures the text `ax continue`'s live-reuse path delivers, and can
// be told to fail delivery so the restore-on-failure path is exercised.
type recordMux struct {
	mux.Multiplexer
	sent    []string
	enter   bool
	failErr error
}

func (m *recordMux) Send(_, text string, enter bool) error {
	if m.failErr != nil {
		return m.failErr
	}
	m.sent = append(m.sent, text)
	m.enter = enter
	return nil
}

// keepLiveActive: an indefinite --keep-live (zero deadline) is always in force; a
// --keep-live-for lease is in force before its deadline and lapses after it; and
// no keep-live is never in force.
func TestKeepLiveActive(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	future := now.Add(5 * time.Minute)
	past := now.Add(-1 * time.Minute)

	if !keepLiveActive(true, time.Time{}, now) {
		t.Error("indefinite keep-live must be active")
	}
	if !keepLiveActive(true, future, now) {
		t.Error("lease before its deadline must be active")
	}
	if keepLiveActive(true, past, now) {
		t.Error("lease past its deadline must not be active")
	}
	if keepLiveActive(false, time.Time{}, now) {
		t.Error("no keep-live is never active")
	}
	if keepLiveActive(false, future, now) {
		t.Error("no keep-live is never active even with a deadline set")
	}
}

// reuseReadyFacts is true only for a live, task-concluded, keep-live interactive
// worker that is idle (not failed/waiting/working). Flipping any single condition
// makes it false, so the list fact and `ax continue`'s accept can never diverge.
func TestReuseReadyFacts(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	live := state.Runtime{State: state.Live, Done: true, Activity: state.Idle}

	if !reuseReadyFacts(live, "interactive", "ship it", true, time.Time{}, now) {
		t.Fatal("a live concluded keep-live idle interactive worker must be reuse-ready")
	}

	cases := []struct {
		name      string
		r         state.Runtime
		mode      string
		task      string
		keepLive  bool
		keepUntil time.Time
	}{
		{"not live (dormant)", state.Runtime{State: "", Done: true, Activity: state.Idle}, "interactive", "ship it", true, time.Time{}},
		{"not done", state.Runtime{State: state.Live, Done: false, Activity: state.Idle}, "interactive", "ship it", true, time.Time{}},
		{"failed", state.Runtime{State: state.Live, Done: true, Failed: true, Activity: state.Idle}, "interactive", "ship it", true, time.Time{}},
		{"working", state.Runtime{State: state.Live, Done: true, Activity: state.Working}, "interactive", "ship it", true, time.Time{}},
		{"waiting on children", state.Runtime{State: state.Live, Done: true, Activity: state.Idle, Waiting: "children"}, "interactive", "ship it", true, time.Time{}},
		{"waiting on input", state.Runtime{State: state.Live, Done: true, Activity: state.Idle, Waiting: "input"}, "interactive", "ship it", true, time.Time{}},
		{"not keep-live", live, "interactive", "ship it", false, time.Time{}},
		{"lease expired", live, "interactive", "ship it", true, now.Add(-time.Minute)},
		{"headless", live, "headless", "ship it", true, time.Time{}},
		{"taskless", live, "interactive", "  ", true, time.Time{}},
	}
	for _, c := range cases {
		if reuseReadyFacts(c.r, c.mode, c.task, c.keepLive, c.keepUntil, now) {
			t.Errorf("%s: expected NOT reuse-ready", c.name)
		}
	}
}

// The live-reuse path reopens the task lifecycle before delivery: it rewrites the
// task, clears the stale outcome/result/exit, removes the terminal marker (so an
// `ax wait` on the new task blocks rather than returning on the stale success),
// preserves keep-live, and delivers the new task through the mux backend.
func TestContinueLiveReuseResetsMarkers(t *testing.T) {
	isolate(t)
	id := "warm-worker"
	exit := 0
	if err := meta.Save(id, meta.Meta{
		Parent: "coord", Mode: "interactive", Task: "old task", Group: "run-1",
		KeepLive: true, Outcome: "success", Result: "old result", Exit: &exit,
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	if !state.Terminal(id) {
		t.Fatal("precondition: worker should be terminal (done) before reuse")
	}

	rm := &recordMux{}
	a := App{mux: rm}
	out := captureStdout(t, func() {
		if err := a.continueLiveReuse(id, "run-1", launchOpts{task: "new task"}); err != nil {
			t.Fatalf("continueLiveReuse: %v", err)
		}
	})

	// On success the identity line is printed to stdout so a script can capture it.
	if !strings.Contains(out, id) {
		t.Errorf("stdout should carry the session id on success, got %q", out)
	}
	if len(rm.sent) != 1 || rm.sent[0] != "new task" {
		t.Fatalf("delivered text = %v, want [new task]", rm.sent)
	}
	if !rm.enter {
		t.Error("task must be submitted (enter), not left unsent")
	}
	m := meta.Load(id)
	if m.Task != "new task" {
		t.Errorf("task = %q, want the new task", m.Task)
	}
	if m.Outcome != "" || m.Result != "" || m.Exit != nil {
		t.Errorf("stale outcome/result/exit not cleared: outcome=%q result=%q exit=%v", m.Outcome, m.Result, m.Exit)
	}
	if !m.KeepLive {
		t.Error("keep-live must be preserved so the worker stays warm for the next reuse")
	}
	if state.Terminal(id) {
		t.Error("terminal marker not cleared: ax wait would return on the stale success instead of tracking the new task")
	}
}

// If delivery fails, the worker is not left reopened-but-untasked: the done marker
// is restored so it reads as concluded (reuse-ready) again, and the error surfaces.
func TestContinueLiveReuseDeliveryFailureRestoresDone(t *testing.T) {
	isolate(t)
	id := "warm-worker-fail"
	if err := meta.Save(id, meta.Meta{Parent: "coord", Mode: "interactive", Task: "old", Group: "r", KeepLive: true}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}

	rm := &recordMux{failErr: errSendBoom}
	a := App{mux: rm}
	var err error
	out := captureStdout(t, func() { err = a.continueLiveReuse(id, "r", launchOpts{task: "new"}) })
	if err == nil {
		t.Fatal("expected an error when delivery fails")
	}
	// stdout must stay empty on a delivery failure: the success line/JSON is emitted
	// only after Send succeeds, so automation parsing stdout never reads success
	// while the caller exits non-zero on the returned error.
	if strings.TrimSpace(out) != "" {
		t.Errorf("stdout must be empty on delivery failure, got %q", out)
	}
	if !state.Done(id) {
		t.Error("done marker not restored after a failed delivery: worker left reopened but untasked")
	}
}

var errSendBoom = errBoom("send failed")

type errBoom string

func (e errBoom) Error() string { return string(e) }

// --keep-live-for parses as a lease: it implies keep-live and records the
// duration; an invalid duration is rejected.
func TestParseKeepLiveFor(t *testing.T) {
	o, err := parseLaunch([]string{"do x", "--keep-live-for", "5m"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !o.keepLive || o.keepLiveFor != "5m" {
		t.Fatalf("keepLive=%v keepLiveFor=%q, want true/5m", o.keepLive, o.keepLiveFor)
	}
	if _, err := parseLaunch([]string{"do x", "--keep-live-for", "nonsense"}); err == nil {
		t.Fatal("an invalid --keep-live-for duration must error")
	}
	if _, err := parseLaunch([]string{"do x", "--keep-live-for", "0s"}); err == nil {
		t.Fatal("a non-positive --keep-live-for duration must error")
	}
}

// keepLiveDeadline resolves the lease into an absolute deadline (now+dur), and is
// the zero time for an indefinite --keep-live or no keep-live at all.
func TestKeepLiveDeadline(t *testing.T) {
	if d := keepLiveDeadline(launchOpts{}); !d.IsZero() {
		t.Errorf("no keep-live: deadline = %v, want zero", d)
	}
	if d := keepLiveDeadline(launchOpts{keepLive: true}); !d.IsZero() {
		t.Errorf("indefinite keep-live: deadline = %v, want zero", d)
	}
	before := time.Now().Add(4 * time.Minute)
	d := keepLiveDeadline(launchOpts{keepLive: true, keepLiveFor: "5m"})
	after := time.Now().Add(6 * time.Minute)
	if d.Before(before) || d.After(after) {
		t.Errorf("lease deadline = %v, want ~now+5m", d)
	}
}

// A --keep-live-for lease schedules the reap for lease expiry (not the normal
// reap_after), so a leased worker is reaped when its lease elapses even if the
// coordinator crashed. An indefinite keep-live schedules nothing. A lease already
// expired by conclude time falls back to the normal delay.
func TestMaybeScheduleWorkerReapLease(t *testing.T) {
	ret := config.Retention{ReapConcludedWorkers: true, ReapAfter: "60s"}
	var gotDelay time.Duration
	var scheduledN int
	orig := workerReaperFn
	workerReaperFn = func(_ string, delay time.Duration) { scheduledN++; gotDelay = delay }
	t.Cleanup(func() { workerReaperFn = orig })

	// Indefinite keep-live: never scheduled.
	scheduledN = 0
	if maybeScheduleWorkerReap("id", meta.Meta{Parent: "c", Mode: "interactive", Task: "t", KeepLive: true}, ret) {
		t.Error("indefinite keep-live must not schedule a reap")
	}
	if scheduledN != 0 {
		t.Error("indefinite keep-live scheduled a reaper anyway")
	}

	// Lease with a future deadline: scheduled at ~lease expiry.
	scheduledN = 0
	deadline := time.Now().Add(3 * time.Minute)
	if !maybeScheduleWorkerReap("id", meta.Meta{Parent: "c", Mode: "interactive", Task: "t", KeepLive: true, KeepUntil: deadline}, ret) {
		t.Fatal("an active lease must schedule a reap at lease end")
	}
	if scheduledN != 1 {
		t.Fatalf("scheduled %d reapers, want 1", scheduledN)
	}
	if gotDelay < 2*time.Minute || gotDelay > 3*time.Minute {
		t.Errorf("lease reap delay = %v, want ~3m (until the deadline), not reap_after", gotDelay)
	}

	// Lease already expired by conclude time: fall back to reap_after.
	scheduledN = 0
	if !maybeScheduleWorkerReap("id", meta.Meta{Parent: "c", Mode: "interactive", Task: "t", KeepLive: true, KeepUntil: time.Now().Add(-time.Minute)}, ret) {
		t.Fatal("an expired lease must still schedule a reap")
	}
	if gotDelay != 60*time.Second {
		t.Errorf("expired-lease reap delay = %v, want reap_after (60s)", gotDelay)
	}
}

// A leased worker is NOT reapable before its deadline but IS after it elapses.
func TestShouldReapWorkerLeaseExpiry(t *testing.T) {
	ret := config.Retention{ReapConcludedWorkers: true, ReapAfter: "60s"}
	active := meta.Meta{Parent: "c", Mode: "interactive", Task: "t", KeepLive: true, KeepUntil: time.Now().Add(2 * time.Minute)}
	if shouldReapWorker(active, ret) {
		t.Error("a worker with an in-force lease must not be reapable yet")
	}
	expired := meta.Meta{Parent: "c", Mode: "interactive", Task: "t", KeepLive: true, KeepUntil: time.Now().Add(-2 * time.Minute)}
	if !shouldReapWorker(expired, ret) {
		t.Error("a worker whose lease has elapsed must become reapable again")
	}
	indefinite := meta.Meta{Parent: "c", Mode: "interactive", Task: "t", KeepLive: true}
	if shouldReapWorker(indefinite, ret) {
		t.Error("an indefinite keep-live worker must never be reapable")
	}
}

// toWire exposes the worker-reuse facts additively: keep_live, keep_until,
// reuse_ready, terminal_at, idle_since. reuse_ready mirrors reuseReadyFacts.
func TestToWireReuseFacts(t *testing.T) {
	last := time.Unix(1_700_000_000, 0)
	term := time.Unix(1_700_000_100, 0)
	s := session.Session{
		ID: "w", Mode: "interactive", Task: "ship it", KeepLive: true, Last: last,
	}
	r := state.Runtime{State: state.Live, Done: true, Activity: state.Idle, TerminalAt: term}

	ws := toWire(s, r)
	if !ws.KeepLive {
		t.Error("keep_live not exposed")
	}
	if !ws.ReuseReady {
		t.Error("reuse_ready should be true for a live concluded keep-live idle worker")
	}
	if !ws.TerminalAt.Equal(term) {
		t.Errorf("terminal_at = %v, want %v", ws.TerminalAt, term)
	}
	if !ws.IdleSince.Equal(last) {
		t.Errorf("idle_since = %v, want the last-activity time %v", ws.IdleSince, last)
	}

	// A non-keep-live done worker is not reuse-ready (it is about to be reaped).
	s.KeepLive = false
	if toWire(s, r).ReuseReady {
		t.Error("a non-keep-live worker must not be reuse-ready")
	}
}

// The schema bump is additive: SchemaVersion is 7, and a report that predates the
// reuse facts (no keep_live/reuse_ready/terminal_at) still decodes, with those
// fields reading as their zero values.
func TestWireSchemaAdditiveReuseFacts(t *testing.T) {
	if wire.SchemaVersion != 7 {
		t.Fatalf("SchemaVersion = %d, want 7", wire.SchemaVersion)
	}
	// A v6-shaped session row: no reuse-fact fields at all.
	old := `{"harness":"claude","id":"w","dir":"/tmp","model":"opus","title":"t","last":"2026-07-05T00:00:00Z","state":"live","activity":"idle","done":true}`
	var s wire.Session
	if err := json.Unmarshal([]byte(old), &s); err != nil {
		t.Fatalf("a pre-v7 report must still decode: %v", err)
	}
	if s.KeepLive || s.ReuseReady || !s.TerminalAt.IsZero() || !s.KeepUntil.IsZero() {
		t.Error("missing reuse-fact fields must decode as their zero values")
	}
	if !s.Done {
		t.Error("existing fields must still decode")
	}

	// Round-trip a v7 row with the reuse facts set.
	term := time.Unix(1_700_000_100, 0).UTC()
	round := wire.Session{ID: "w", KeepLive: true, ReuseReady: true, TerminalAt: term}
	b, err := json.Marshal(round)
	if err != nil {
		t.Fatal(err)
	}
	var back wire.Session
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.KeepLive || !back.ReuseReady || !back.TerminalAt.Equal(term) {
		t.Errorf("reuse facts did not round-trip: %+v", back)
	}
}
