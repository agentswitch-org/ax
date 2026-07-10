package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/state"
)

// stubCloseSession replaces the Stop-hook teardown spawn for a test: the real
// closeSession execs this binary as `ax await-close`, which under `go test`
// would re-run the test suite itself. Records the ids closed for assertions.
func stubCloseSession(t *testing.T) *[]string {
	t.Helper()
	var closed []string
	orig := closeSessionFn
	closeSessionFn = func(id string) { closed = append(closed, id) }
	t.Cleanup(func() { closeSessionFn = orig })
	return &closed
}

type reaperMux struct {
	mux.Multiplexer
	closed   []string
	closeErr error
	windows  map[string]string
}

func (m *reaperMux) Active() bool     { return true }
func (m *reaperMux) HasWindows() bool { return true }

func (m *reaperMux) Live() map[string]string { return m.windows }

func (m *reaperMux) CloseWindow(id string) error {
	m.closed = append(m.closed, id)
	return m.closeErr
}

func stubWorkerReapKill(t *testing.T) *[]string {
	t.Helper()
	var killed []string
	orig := workerReapKillFn
	workerReapKillFn = func(id string) error {
		killed = append(killed, id)
		return nil
	}
	t.Cleanup(func() { workerReapKillFn = orig })
	return &killed
}

// TestConcludable pins which launches conclude on task-done: only a task-carrying
// watched (interactive) worker. A taskless interactive session a human is driving
// and a headless job both stay out of the conclude path.
func TestConcludable(t *testing.T) {
	cases := []struct {
		name string
		m    meta.Meta
		want bool
	}{
		{"interactive worker with task", meta.Meta{Mode: "interactive", Task: "do x"}, true},
		{"interactive, no task (human driving)", meta.Meta{Mode: "interactive"}, false},
		{"interactive, whitespace task", meta.Meta{Mode: "interactive", Task: "  \n"}, false},
		{"headless job with task", meta.Meta{Mode: "headless", Task: "do x"}, false},
		{"zero meta", meta.Meta{}, false},
	}
	for _, c := range cases {
		if got := concludable(c.m); got != c.want {
			t.Errorf("%s: concludable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestShouldReapWorkerPredicate(t *testing.T) {
	ret := config.Retention{ReapConcludedWorkers: true, ReapAfter: "60s"}
	activeLease := time.Now().Add(time.Hour)
	base := meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it"}
	if !shouldReapWorker(base, ret) {
		t.Fatal("parented interactive task worker should be reaped")
	}
	legacy := meta.Meta{Group: "run1", Origin: "agent", Mode: "interactive", Task: "ship it", Labels: []string{"role=worker"}}
	if !shouldReapWorker(legacy, ret) {
		t.Fatal("legacy unparented tracked worker should be reaped")
	}
	legacyHumanWorker := meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=worker"}}
	if !shouldReapWorker(legacyHumanWorker, ret) {
		t.Fatal("legacy human-origin tracked worker should be reaped")
	}
	legacyHumanReviewer := meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=reviewer"}}
	if !shouldReapWorker(legacyHumanReviewer, ret) {
		t.Fatal("legacy human-origin tracked reviewer should be reaped")
	}
	for _, tt := range []struct {
		name string
		m    meta.Meta
		ret  config.Retention
	}{
		{"parentless", meta.Meta{Mode: "interactive", Task: "ship it"}, ret},
		{"tracked role without group", meta.Meta{Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=worker"}}, ret},
		{"parentless human without tracked role", meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it"}, ret},
		{"legacy human wrong role", meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=lead"}}, ret},
		{"parentless unroled task", meta.Meta{Group: "run1", Origin: "agent", Mode: "interactive", Task: "ship it"}, ret},
		{"taskless", meta.Meta{Parent: "coord", Mode: "interactive"}, ret},
		{"headless", meta.Meta{Parent: "coord", Mode: "headless", Task: "ship it"}, ret},
		{"keep-live", meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it", KeepLive: true}, ret},
		{"legacy human tracked keep-live", meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=reviewer"}, KeepLive: true}, ret},
		{"legacy human active keep-live lease", meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=worker"}, KeepLive: true, KeepUntil: activeLease}, ret},
		{"config disabled", base, config.Retention{ReapConcludedWorkers: false, ReapAfter: "60s"}},
	} {
		if shouldReapWorker(tt.m, tt.ret) {
			t.Fatalf("%s should not be reaped", tt.name)
		}
	}
}

func TestReapWorkerClosesWindowForDoneResidentWorker(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000000101"
	writeClaudeTranscript(t, home, id, "done and idle")
	if err := meta.Save(id, meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	writeLegacyLive(t, id, "ax run")
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{}

	App{mux: mx}.ReapWorker([]string{id, "0s"})

	if len(mx.closed) != 1 || mx.closed[0] != id {
		t.Fatalf("closed windows = %v, want [%s]", mx.closed, id)
	}
	if len(*killed) != 1 || (*killed)[0] != id {
		t.Fatalf("reap kill calls = %v, want [%s]", *killed, id)
	}
}

func TestReapWorkerSkipsUnreapableWindows(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		setup func(t *testing.T, id string)
	}{
		{
			name: "active working",
			id:   "00000000-0000-0000-0000-000000000201",
			setup: func(t *testing.T, id string) {
				if err := meta.Save(id, meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it"}); err != nil {
					t.Fatal(err)
				}
				writeLegacyLive(t, id, "ax run")
				if err := state.WriteHook(id, state.Working); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "waiting on children",
			id:   "00000000-0000-0000-0000-000000000202",
			setup: func(t *testing.T, id string) {
				if err := meta.Save(id, meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it"}); err != nil {
					t.Fatal(err)
				}
				writeLegacyLive(t, id, "ax run")
				if err := state.WriteHook(id, "done"); err != nil {
					t.Fatal(err)
				}
				if err := state.MarkWaiting(id, []string{"child-still-running"}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "waiting on human ask",
			id:   "00000000-0000-0000-0000-000000000203",
			setup: func(t *testing.T, id string) {
				if err := meta.Save(id, meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it"}); err != nil {
					t.Fatal(err)
				}
				writeLegacyLive(t, id, "ax run")
				if err := state.WriteHook(id, "done"); err != nil {
					t.Fatal(err)
				}
				if err := ask.Save(id, ask.Pending{Question: "continue?"}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "keep live",
			id:   "00000000-0000-0000-0000-000000000204",
			setup: func(t *testing.T, id string) {
				if err := meta.Save(id, meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it", KeepLive: true}); err != nil {
					t.Fatal(err)
				}
				writeLegacyLive(t, id, "ax run")
				if err := state.WriteHook(id, "done"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := isolate(t)
			id := tc.id
			writeClaudeTranscript(t, home, id, tc.name)
			tc.setup(t, id)
			killed := stubWorkerReapKill(t)
			mx := &reaperMux{}

			App{mux: mx}.ReapWorker([]string{id, "0s"})

			if len(mx.closed) != 0 {
				t.Fatalf("closed unreapable window(s): %v", mx.closed)
			}
			if len(*killed) != 0 {
				t.Fatalf("killed unreapable session(s): %v", *killed)
			}
		})
	}
}

func TestReapWorkerWindowCleanupAlreadyGoneIsSafe(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000000301"
	writeClaudeTranscript(t, home, id, "done and idle")
	if err := meta.Save(id, meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	writeLegacyLive(t, id, "ax run")
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{closeErr: errors.New("no window running worker")}

	App{mux: mx}.ReapWorker([]string{id, "0s"})

	if len(mx.closed) != 1 || mx.closed[0] != id {
		t.Fatalf("closed windows = %v, want one best-effort close for %s", mx.closed, id)
	}
	if len(*killed) != 1 || (*killed)[0] != id {
		t.Fatalf("window close error blocked reap kill: got %v, want [%s]", *killed, id)
	}
}

func TestWorkerReapPreservesTranscriptMetaAndHook(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	id := "worker-reap"
	transcript := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := meta.Save(id, meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	var scheduled []string
	var gotDelay time.Duration
	orig := workerReaperFn
	workerReaperFn = func(sid string, delay time.Duration) {
		scheduled = append(scheduled, sid)
		gotDelay = delay
	}
	t.Cleanup(func() { workerReaperFn = orig })

	if !maybeScheduleWorkerReap(id, meta.Load(id), config.Retention{ReapConcludedWorkers: true, ReapAfter: "0s"}) {
		t.Fatal("expected reap to schedule")
	}
	if len(scheduled) != 1 || scheduled[0] != id || gotDelay != 0 {
		t.Fatalf("scheduled reap = %v delay=%s, want %s/0", scheduled, gotDelay, id)
	}
	if _, err := os.Stat(transcript); err != nil {
		t.Fatalf("reap scheduling removed transcript: %v", err)
	}
	if got := meta.Load(id); got.Parent != "coord" || got.Task != "ship it" {
		t.Fatalf("reap scheduling mutated meta: %+v", got)
	}
	if !state.Done(id) {
		t.Fatal("reap scheduling cleared the done hook marker")
	}
}

func TestReapWorkerRechecksStateAtFireTime(t *testing.T) {
	home := isolate(t)
	var killed []string
	orig := workerReapKillFn
	workerReapKillFn = func(id string) error {
		killed = append(killed, id)
		return nil
	}
	t.Cleanup(func() { workerReapKillFn = orig })

	resumed := "00000000-0000-0000-0000-000000000def"
	writeClaudeTranscript(t, home, resumed, "done before resume")
	if err := meta.Save(resumed, meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	writeLegacyLive(t, resumed, "ax run")
	if err := state.WriteHook(resumed, "done"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		App{}.ReapWorker([]string{resumed, "30ms"})
		close(done)
	}()
	time.Sleep(5 * time.Millisecond)
	if err := state.WriteHook(resumed, "working"); err != nil {
		t.Fatal(err)
	}
	writeLegacyLive(t, resumed, "ax run")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reaper did not return")
	}
	if len(killed) != 0 {
		t.Fatalf("resumed worker was killed at reap fire time: %v", killed)
	}

	stillDone := "00000000-0000-0000-0000-000000000fed"
	writeClaudeTranscript(t, home, stillDone, "done and idle")
	if err := meta.Save(stillDone, meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	writeLegacyLive(t, stillDone, "ax run")
	if err := state.WriteHook(stillDone, "done"); err != nil {
		t.Fatal(err)
	}
	App{}.ReapWorker([]string{stillDone, "0s"})
	if len(killed) != 1 || killed[0] != stillDone {
		t.Fatalf("still idle done-resident worker kill calls = %v, want [%s]", killed, stillDone)
	}
}

func TestReapWorkerReopensHooklessTurnAtFireTime(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000000c0d"
	markerAt := time.Now().Add(-time.Minute)
	writeCodexLifecycleTranscript(t, home, id, markerAt)
	exit := 1
	if err := meta.Save(id, meta.Meta{
		Parent:     "coord",
		Mode:       "interactive",
		Harness:    "codex",
		Task:       "ship it",
		Outcome:    "failure",
		FailReason: "turn aborted",
		Result:     "old failure",
		Exit:       &exit,
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "failed"); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(axdir.StatePath("hookstate"), id)
	if err := os.Chtimes(hook, markerAt, markerAt); err != nil {
		t.Fatal(err)
	}
	writeFreshLegacyLiveRecord(t, id)
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{}

	App{mux: mx}.ReapWorker([]string{id, "0s"})

	if len(mx.closed) != 0 {
		t.Fatalf("closed resumed worker window: %v", mx.closed)
	}
	if len(*killed) != 0 {
		t.Fatalf("killed resumed worker at reap fire time: %v", *killed)
	}
	if state.Terminal(id) {
		t.Fatal("terminal marker still present after resumed turn was detected")
	}
	m := meta.Load(id)
	if m.Outcome != "" || m.FailReason != "" || m.Result != "" || m.Exit != nil {
		t.Fatalf("stale terminal meta not cleared: outcome=%q reason=%q result=%q exit=%v", m.Outcome, m.FailReason, m.Result, m.Exit)
	}
}

// TestConcludeOrIdle covers the turn-end (Stop) choke point: a task worker
// transitions into the done state, a taskless session just goes idle, and
// --close-on-done tears the session's sidecars down instead of halting in done.
func TestConcludeOrIdle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir) // isolate: no user notify config fires
	a := App{}
	stubCloseSession(t)

	// A task-carrying interactive worker concludes into the visible done state.
	worker := "worker-1"
	if err := meta.Save(worker, meta.Meta{Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	a.concludeOrIdle(worker)
	if !state.Done(worker) {
		t.Fatal("task worker did not transition into the done state on stop")
	}
	if hs, _ := state.HookState(worker); hs != "done" {
		t.Fatalf("worker hook state = %q, want done", hs)
	}

	// A taskless interactive session a human drives keeps the plain idle behavior.
	human := "human-1"
	if err := meta.Save(human, meta.Meta{Mode: "interactive"}); err != nil {
		t.Fatal(err)
	}
	a.concludeOrIdle(human)
	if state.Done(human) {
		t.Fatal("taskless human session must not conclude")
	}
	if hs, _ := state.HookState(human); hs != state.Idle {
		t.Fatalf("human hook state = %q, want idle", hs)
	}

	// --close-on-done ends the session, but the done state it just concluded
	// into must survive that teardown (not be wiped back to no hook state),
	// so the picker still reads a concluded worker instead of a corpse, with a
	// success outcome recorded. live.Kill on an unknown id is a safe no-op.
	closer := "closer-1"
	if err := meta.Save(closer, meta.Meta{Mode: "interactive", Task: "ship it", CloseOnDone: true}); err != nil {
		t.Fatal(err)
	}
	// A stale pending question must still be cleared on teardown (a dead
	// session must not keep asserting "needs you"), even though the done hook
	// state now survives.
	if err := ask.Save(closer, ask.Pending{Question: "proceed?"}); err != nil {
		t.Fatal(err)
	}
	a.concludeOrIdle(closer)
	if !state.Done(closer) {
		t.Fatal("close-on-done must leave the session in the done state, not clear it")
	}
	if got := meta.Load(closer).Outcome; got != "success" {
		t.Fatalf("close-on-done outcome = %q, want success", got)
	}
	if _, ok := ask.Load(closer); ok {
		t.Fatal("close-on-done must still clear a stale pending question on teardown")
	}
}

// TestConcludeExit covers the run wrapper's universal exit-time conclusion: by
// the time a session's process is gone, every task-carrying launch must hold a
// durable terminal marker. A clean headless exit concludes done; a non-zero
// headless exit concludes failed with a reason from its last output; an
// interactive worker whose process died without its turn-end ever firing (a
// crash, a quit, an ax-initiated stop) concludes failed instead of leaving a
// waiter hanging forever; and a session that already concluded is never
// overwritten by its own teardown exit.
func TestConcludeExit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	// A zero exit on a headless job: done, outcome success, never failed.
	ok := "job-ok"
	if err := meta.Save(ok, meta.Meta{Mode: "headless", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	ConcludeExit(ok, 0, "all good\n", false)
	if !state.Done(ok) {
		t.Fatal("headless job did not transition into the done state on a clean exit")
	}
	if state.Failed(ok) {
		t.Fatal("a clean exit must not be marked failed")
	}
	if got := meta.Load(ok).Outcome; got != "success" {
		t.Fatalf("outcome = %q, want success", got)
	}

	// A zero exit on a recipe root is the recipe's successful script
	// completion, not an interactive worker disappearing before a Stop hook.
	recipeOK := "recipe-ok"
	if err := meta.Save(recipeOK, meta.Meta{Mode: "recipe", Task: "/tmp/recipe.sh"}); err != nil {
		t.Fatal(err)
	}
	ConcludeExit(recipeOK, 0, "recipe done\n", false)
	if !state.Done(recipeOK) || state.Failed(recipeOK) {
		t.Fatal("clean recipe exit must conclude done, not failed")
	}
	if got := meta.Load(recipeOK).Outcome; got != "success" {
		t.Fatalf("recipe outcome = %q, want success", got)
	}

	// A non-zero exit on a headless job: failed (not done), outcome failure, and
	// a short reason captured from the tail of its own output.
	bad := "job-bad"
	if err := meta.Save(bad, meta.Meta{Mode: "headless", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	ConcludeExit(bad, 1, "doing work\n\nError: something broke\n", false)
	if state.Done(bad) {
		t.Fatal("a non-zero exit must not be marked done")
	}
	if !state.Failed(bad) {
		t.Fatal("headless job did not transition into the failed state on a non-zero exit")
	}
	if got := meta.Load(bad).Outcome; got != "failure" {
		t.Fatalf("outcome = %q, want failure", got)
	}
	if got := meta.Load(bad).FailReason; got != "Error: something broke" {
		t.Fatalf("fail reason = %q, want the last non-blank line of the tail", got)
	}

	// An interactive task worker whose process exited WITHOUT ever concluding
	// (its Stop hook / turn-end never fired: a crash, a manual quit) must land
	// in the failed state, or a waiter on its id blocks forever on a corpse.
	dead := "worker-dead"
	if err := meta.Save(dead, meta.Meta{Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	ConcludeExit(dead, 1, "panic: boom\n", false)
	if !state.Failed(dead) {
		t.Fatal("an interactive worker that died before concluding must be marked failed")
	}
	if got := meta.Load(dead).Outcome; got != "failure" {
		t.Fatalf("outcome = %q, want failure", got)
	}

	// An interactive worker that ALREADY concluded (its done marker is durable)
	// exits during --close-on-done teardown: the exit must be an idempotent
	// no-op, never a demotion of success to failure. This is the exact bug that
	// made a successful close-on-done worker read as a failure.
	closed := "worker-closed"
	if err := meta.Save(closed, meta.Meta{Mode: "interactive", Task: "ship it", Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	state.WriteHook(closed, "done")
	ConcludeExit(closed, -1, "", true) // force-killed teardown exit
	if !state.Done(closed) || state.Failed(closed) {
		t.Fatal("a concluded worker's teardown exit must not overwrite its done state")
	}
	if got := meta.Load(closed).Outcome; got != "success" {
		t.Fatalf("outcome = %q, want success preserved through teardown", got)
	}

	// An ax-initiated stop (kill, fence trip, restart) of a not-yet-concluded
	// task worker concludes failed with a stable reason, both modes.
	stoppedJob := "job-stopped"
	if err := meta.Save(stoppedJob, meta.Meta{Mode: "headless", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	ConcludeExit(stoppedJob, 0, "", true)
	if !state.Failed(stoppedJob) {
		t.Fatal("a stopped headless job must conclude failed, not linger non-terminal")
	}
	if got := meta.Load(stoppedJob).FailReason; got != "stopped before concluding" {
		t.Fatalf("fail reason = %q, want stopped before concluding", got)
	}

	// A taskless interactive session (a human at the wheel) is never concluded
	// by its own exit, and an id with no meta (or empty) is a safe no-op.
	human := "human-1"
	if err := meta.Save(human, meta.Meta{Mode: "interactive"}); err != nil {
		t.Fatal(err)
	}
	ConcludeExit(human, 1, "boom", false)
	if state.Failed(human) || state.Done(human) {
		t.Fatal("a taskless human session must not conclude via the exit path")
	}
	ConcludeExit("no-such-session", 0, "", false)
	if state.Done("no-such-session") {
		t.Fatal("an id with no meta must not be marked done")
	}
	ConcludeExit("", 1, "boom", false)
}

// TestKillPreservesConclusion pins the durability contract around kills: a
// concluded worker's terminal marker survives killCleanup (a later cleanup
// kill of its lingering window must not turn success into a corpse), while a
// not-yet-concluded task worker that gets killed is marked failed so a waiter
// unblocks with a truthful outcome.
func TestKillPreservesConclusion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	// A done worker killed afterwards: marker survives, outcome stays success.
	done := "worker-done"
	if err := meta.Save(done, meta.Meta{Mode: "interactive", Task: "ship it", Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	state.WriteHook(done, "done")
	MarkKilled(done)
	killCleanup(done)
	if !state.Done(done) {
		t.Fatal("killCleanup wiped a concluded worker's done marker")
	}
	if got := meta.Load(done).Outcome; got != "success" {
		t.Fatalf("outcome = %q, want success preserved through a kill", got)
	}

	// A running (non-terminal) task worker killed mid-task: failed, reason killed.
	running := "worker-running"
	if err := meta.Save(running, meta.Meta{Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	state.WriteHook(running, "working")
	MarkKilled(running)
	killCleanup(running)
	if !state.Failed(running) {
		t.Fatal("a killed non-concluded task worker must be marked failed")
	}
	if got := meta.Load(running).FailReason; got != "killed" {
		t.Fatalf("fail reason = %q, want killed", got)
	}

	// Non-terminal activity states are still cleared (a corpse must not keep
	// asserting working), and a taskless session is never marked failed.
	human := "human-2"
	if err := meta.Save(human, meta.Meta{Mode: "interactive"}); err != nil {
		t.Fatal(err)
	}
	state.WriteHook(human, "working")
	MarkKilled(human)
	killCleanup(human)
	if _, ok := state.HookState(human); ok {
		t.Fatal("killCleanup must clear a non-terminal activity state")
	}
	if state.Failed(human) {
		t.Fatal("a taskless session must not be marked failed by a kill")
	}
}

// TestConcludeTurnEnd covers the transcript-watcher done-gate for harnesses
// with no lifecycle hook (pi, codex): a clean turn end concludes exactly like
// claude's Stop hook, an error turn end concludes failed with the reason, and
// a taskless session is untouched.
func TestConcludeTurnEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir) // isolate: no user notify config fires

	worker := "pi-worker"
	if err := meta.Save(worker, meta.Meta{Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	ConcludeTurnEnd(worker, "")
	if !state.Done(worker) {
		t.Fatal("turn end did not conclude the worker into the done state")
	}

	errWorker := "pi-error"
	if err := meta.Save(errWorker, meta.Meta{Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	ConcludeTurnEnd(errWorker, "model error")
	if !state.Failed(errWorker) {
		t.Fatal("an error turn end must conclude failed")
	}
	if got := meta.Load(errWorker).FailReason; got != "model error" {
		t.Fatalf("fail reason = %q, want model error", got)
	}

	// An error turn end never demotes a worker that already concluded done.
	ConcludeTurnEnd(worker, "model error")
	if !state.Done(worker) || state.Failed(worker) {
		t.Fatal("an error turn end must not demote an already-done worker")
	}

	human := "pi-human"
	if err := meta.Save(human, meta.Meta{Mode: "interactive"}); err != nil {
		t.Fatal(err)
	}
	ConcludeTurnEnd(human, "")
	if state.Done(human) {
		t.Fatal("a taskless session must not conclude on turn end")
	}
}

// TestConcludeExitKeepsEarlyReason: when a fatal pattern already marked a
// run failed (MarkFailed) before it exited, the later exit-code path must not
// clobber that more specific reason with the generic exit tail.
func TestConcludeExitKeepsEarlyReason(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	id := "job-early"
	if err := meta.Save(id, meta.Meta{Mode: "headless", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	MarkFailed(id, "unsupported model")
	ConcludeExit(id, 1, "harness exiting after fatal error\n", false)
	if got := meta.Load(id).FailReason; got != "unsupported model" {
		t.Fatalf("fail reason = %q, want the early-detected reason preserved", got)
	}
}

// TestMarkFailed covers the early-detection path: a headless run is marked
// failed the instant a known-fatal pattern shows up in its own output, without
// waiting for the process to exit, and this never applies to an interactive
// session or a session already terminal.
func TestMarkFailed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	id := "job-fatal"
	if err := meta.Save(id, meta.Meta{Mode: "headless", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	MarkFailed(id, "unsupported model")
	if !state.Failed(id) {
		t.Fatal("headless job did not transition into the failed state on a fatal pattern")
	}
	if got := meta.Load(id).Outcome; got != "failure" {
		t.Fatalf("outcome = %q, want failure", got)
	}
	if got := meta.Load(id).FailReason; got != "unsupported model" {
		t.Fatalf("fail reason = %q, want unsupported model", got)
	}

	// A session already done is never regressed to failed.
	concluded := "job-done"
	if err := meta.Save(concluded, meta.Meta{Mode: "headless", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	ConcludeExit(concluded, 0, "ok", false)
	MarkFailed(concluded, "unsupported model")
	if state.Failed(concluded) {
		t.Fatal("MarkFailed must not regress an already-done session")
	}

	// An interactive session is never marked failed through this path.
	interactive := "human-2"
	if err := meta.Save(interactive, meta.Meta{Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	MarkFailed(interactive, "unsupported model")
	if state.Failed(interactive) {
		t.Fatal("an interactive session must not be marked failed by MarkFailed")
	}

	// An id with no meta at all (or an empty id) is a safe no-op.
	MarkFailed("no-such-session", "unsupported model")
	if state.Failed("no-such-session") {
		t.Fatal("an id with no meta must not be marked failed")
	}
	MarkFailed("", "unsupported model")
}

// TestFatalReason pins the known-fatal error signatures a headless run's early
// output is scanned for: an unsupported model, a rejected API request, an
// exhausted credit balance, and a missing login/auth, each surfaced with a
// short stable reason instead of the raw (often noisy) error text.
func TestFatalReason(t *testing.T) {
	cases := []struct {
		name       string
		output     string
		wantReason string
		wantOK     bool
	}{
		{"unsupported model", "Error: model 'gpt-99' not found", "unsupported model", true},
		{"model not supported", "model claude-x is not supported by this endpoint", "unsupported model", true},
		{"invalid request", `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`, "invalid request", true},
		{"credit balance", "Your credit balance is too low to access the API", "credit balance too low", true},
		{"auth required", "Error: login required, run `codex login`", "authentication required", true},
		{"invalid api key", "Error: Invalid API key provided", "authentication required", true},
		{"normal output, no match", "Reading files...\nWriting patch...\n", "", false},
	}
	for _, c := range cases {
		reason, ok := FatalReason(c.output)
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.wantOK)
			continue
		}
		if ok && reason != c.wantReason {
			t.Errorf("%s: reason = %q, want %q", c.name, reason, c.wantReason)
		}
	}
}

// TestInstallClaudeHookSelfHeals verifies re-running install migrates a stale ax
// hookstate verb (a pre-conclude `ax hookstate idle` on Stop) to the current one
// (`ax hookstate stop`) without duplicating it, and never touches a user's own hook.
func TestInstallClaudeHookSelfHeals(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads USERPROFILE on Windows
	cfgDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed an old ax hook on Stop plus a user's own Stop hook that must survive.
	seed := map[string]any{"hooks": map[string]any{
		"Stop": []any{
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "ax hookstate idle"}}},
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "my-own-logger"}}},
		},
	}}
	data, _ := json.MarshalIndent(seed, "", "  ")
	path := filepath.Join(cfgDir, "settings.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := installClaudeHook(); err != nil {
		t.Fatal(err)
	}
	if err := installClaudeHook(); err != nil { // idempotent: a second run changes nothing
		t.Fatal(err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	cmds := stopCommands(t, got)
	assertHas(t, cmds, "ax hookstate stop")
	assertHas(t, cmds, "my-own-logger")
	assertLacks(t, cmds, "ax hookstate idle")
	if n := countOf(cmds, "ax hookstate stop"); n != 1 {
		t.Fatalf("ax hookstate stop appears %d times, want exactly 1", n)
	}
}

func stopCommands(t *testing.T, settings map[string]any) []string {
	t.Helper()
	hooks, _ := settings["hooks"].(map[string]any)
	list, _ := hooks["Stop"].([]any)
	var out []string
	for _, g := range list {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, ok := hm["command"].(string); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

func countOf(xs []string, want string) int {
	n := 0
	for _, x := range xs {
		if x == want {
			n++
		}
	}
	return n
}

func assertHas(t *testing.T, xs []string, want string) {
	t.Helper()
	if countOf(xs, want) == 0 {
		t.Fatalf("missing %q in %v", want, xs)
	}
}

func assertLacks(t *testing.T, xs []string, bad string) {
	t.Helper()
	if countOf(xs, bad) != 0 {
		t.Fatalf("stale %q still present in %v", bad, xs)
	}
}
