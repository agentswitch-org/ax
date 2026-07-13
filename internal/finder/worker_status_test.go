package finder

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
	"github.com/agentswitch-org/ax/internal/view"
)

// TestBuildMetaWorkerAttention pins worker attention state. A blocked hook on a
// live worker still renders as done for the owner, but a pending ax ask is
// blocked input until a real Done/Failed marker arrives and must not drive the
// lifecycle to concluded.
func TestBuildMetaWorkerAttention(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// A locator makes a session read Live without a heartbeat, which is the
	// precondition the blocked-hook branch checks.
	live := func(id string) map[string]string { return map[string]string{id: "s:0.0"} }

	t.Run("blocked worker resolves to done", func(t *testing.T) {
		s := session.Session{ID: "w1", Parent: "coord"}
		state.WriteHook("w1", "blocked")
		m := BuildMeta([]session.Session{s}, live("w1"), nil)["w1"]
		if m.Waiting != "" {
			t.Fatalf("worker Waiting = %q, want no needs-you state", m.Waiting)
		}
		if !m.Done {
			t.Fatal("a finished worker must render as done")
		}
		if m.DisplayPhase != view.PhaseLiveDoneResident {
			t.Fatalf("worker display phase = %q, want %q", m.DisplayPhase, view.PhaseLiveDoneResident)
		}
	})

	t.Run("blocked top-level still needs you", func(t *testing.T) {
		s := session.Session{ID: "top1"} // empty Parent
		state.WriteHook("top1", "blocked")
		m := BuildMeta([]session.Session{s}, live("top1"), nil)["top1"]
		if m.Waiting != "input" {
			t.Fatalf("top-level Waiting = %q, want input (needs you)", m.Waiting)
		}
		if m.Done {
			t.Fatal("a blocked top-level session must not read as done")
		}
	})

	t.Run("pending ask: top-level and worker need input", func(t *testing.T) {
		if err := ask.Save("top2", ask.Pending{Question: "which?"}); err != nil {
			t.Fatal(err)
		}
		if err := ask.Save("w2", ask.Pending{Question: "which?"}); err != nil {
			t.Fatal(err)
		}
		sessions := []session.Session{{ID: "top2"}, {ID: "w2", Parent: "coord"}}
		meta := BuildMeta(sessions, nil, nil)

		if meta["top2"].Waiting != "input" {
			t.Fatalf("top-level pending ask Waiting = %q, want input", meta["top2"].Waiting)
		}
		if meta["w2"].Waiting != "input" {
			t.Fatalf("worker pending ask Waiting = %q, want input", meta["w2"].Waiting)
		}
		if meta["w2"].Done || meta["w2"].Lifecycle == state.LifecycleConcluded {
			t.Fatalf("worker pending ask done=%v lifecycle=%q, want blocked and not concluded", meta["w2"].Done, meta["w2"].Lifecycle)
		}
	})
}

// TestBuildMetaSupervisorWaitsOnWorkers pins the fix for the needs-you false
// positive: a top-level owner that fired a blocked hook but still has a
// live descendant worker is supervising, not stuck, so it reads "waiting on
// workers" (calm), never "needs you". A real human ask still overrides that, and
// an owner with no live children still needs the human.
func TestBuildMetaSupervisorWaitsOnWorkers(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	live := func(ids ...string) map[string]string {
		out := map[string]string{}
		for i, id := range ids {
			out[id] = fmt.Sprintf("s:0.%d", i)
		}
		return out
	}

	t.Run("blocked owner with live child waits on workers", func(t *testing.T) {
		state.WriteHook("coord", "blocked")
		sessions := []session.Session{
			{ID: "coord", Group: "run1"},
			{ID: "w1", Group: "run1", Parent: "coord"},
		}
		m := BuildMeta(sessions, live("coord", "w1"), nil)["coord"]
		if m.Waiting == "input" {
			t.Fatalf("supervising owner Waiting = input (needs you), want waiting on workers")
		}
		if m.Waiting != "children" {
			t.Fatalf("supervising owner Waiting = %q, want children (waiting)", m.Waiting)
		}
	})

	// The exact c87700b8 shape: an owner whose blocked hook has aged out
	// (Blocked stays true, it is a durable marker) but whose transcript keeps its
	// activity working, so it reads working-live in the LIFE column. It must still
	// not badge needs-you while its worker is live.
	t.Run("working-live owner with live child is not needs-you", func(t *testing.T) {
		tf := filepath.Join(t.TempDir(), "coord.jsonl")
		if err := os.WriteFile(tf, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := os.Chtimes(tf, now, now); err != nil {
			t.Fatal(err)
		}
		if err := state.WriteHook("c87700b8", "blocked"); err != nil {
			t.Fatal(err)
		}
		// Age the blocked hook past the freshness window so activity falls back to
		// the recent transcript (working) while Blocked stays true.
		stale := now.Add(-2 * state.HookFresh)
		if err := os.Chtimes(filepath.Join(axdir.State("hookstate"), "c87700b8"), stale, stale); err != nil {
			t.Fatal(err)
		}
		sessions := []session.Session{
			{ID: "c87700b8", Group: "chautauqua", File: tf},
			{ID: "w1", Group: "chautauqua", Parent: "c87700b8"},
		}
		m := BuildMeta(sessions, live("c87700b8", "w1"), nil)["c87700b8"]
		if m.DisplayPhase != view.PhaseLiveWorking {
			t.Fatalf("owner display phase = %q, want %q (working-live)", m.DisplayPhase, view.PhaseLiveWorking)
		}
		if m.Waiting == "input" {
			t.Fatalf("working-live supervising owner badged needs-you (Waiting=input)")
		}
		if m.Waiting != "children" {
			t.Fatalf("working-live supervising owner Waiting = %q, want children", m.Waiting)
		}
	})

	t.Run("blocked owner with no live child still needs you", func(t *testing.T) {
		state.WriteHook("lonely", "blocked")
		// The child exists but is not live (no locator, no heartbeat): a dead
		// worker does not make its owner a supervisor.
		sessions := []session.Session{
			{ID: "lonely", Group: "run2"},
			{ID: "deadkid", Group: "run2", Parent: "lonely"},
		}
		m := BuildMeta(sessions, live("lonely"), nil)["lonely"]
		if m.Waiting != "input" {
			t.Fatalf("owner with no live child Waiting = %q, want input (needs you)", m.Waiting)
		}
	})

	t.Run("pending ask surfaces even with a live child", func(t *testing.T) {
		if err := ask.Save("asker", ask.Pending{Question: "which branch?"}); err != nil {
			t.Fatal(err)
		}
		sessions := []session.Session{
			{ID: "asker", Group: "run3"},
			{ID: "w1", Group: "run3", Parent: "asker"},
		}
		m := BuildMeta(sessions, live("asker", "w1"), nil)["asker"]
		if m.Waiting != "input" {
			t.Fatalf("owner with a pending ask Waiting = %q, want input (needs you) even with live children", m.Waiting)
		}
	})
}

func TestBuildMetaInvalidLiveRecordNotWorking(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := "old-claude"
	tf := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(tf, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(tf, now, now); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "working"); err != nil {
		t.Fatal(err)
	}
	writeInvalidLiveRecordForTest(t, id, now)

	s := session.Session{ID: id, File: tf}
	m := BuildMeta([]session.Session{s}, nil, nil)[id]
	if m.State == view.StateLive {
		t.Fatalf("invalid live record produced live RowMeta: %+v", m)
	}
	if m.Activity == view.Working {
		t.Fatalf("invalid live record produced working RowMeta: %+v", m)
	}
	p := &picker{scope: scopeWorking}
	if p.inScope(s, m) {
		t.Fatalf("invalid live record passed working scope: %+v", m)
	}
}

func TestBuildMetaOwnerWaitMarker(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.MarkWaiting("coord", []string{"child"}); err != nil {
		t.Fatal(err)
	}
	sessions := []session.Session{
		{ID: "coord", Group: "run1"},
		{ID: "child", Group: "run1", Parent: "coord"},
	}
	loc := map[string]string{"coord": "s:0.0"}
	meta := BuildMeta(sessions, loc, nil)
	if meta["coord"].Waiting != "children" {
		t.Fatalf("owner Waiting = %q, want children", meta["coord"].Waiting)
	}
	if meta["coord"].DisplayPhase != view.PhaseLiveWaiting {
		t.Fatalf("owner display phase = %q, want %q", meta["coord"].DisplayPhase, view.PhaseLiveWaiting)
	}
	p := &picker{
		all:       sessions,
		meta:      meta,
		scope:     scopeWorking,
		groupBy:   "run",
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		km:        keys.Build(nil),
	}
	p.recompute()
	if len(p.matches) == 0 {
		t.Fatal("working scope should keep an owner waiting on non-terminal children")
	}

	if err := state.WriteHook("child", "done"); err != nil {
		t.Fatal(err)
	}
	meta = BuildMeta(sessions, loc, nil)
	if meta["coord"].Waiting == "children" {
		t.Fatal("owner should stop waiting once every marked child is terminal")
	}
}

func TestBuildMetaDeadFreshHeartbeatDoesNotRenderWorking(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := "dead-done"
	deadPID := 99999999
	rec := fmt.Sprintf("%d\t%d\tax run --dangerously-skip-permissions", time.Now().Unix(), deadPID)
	if err := os.WriteFile(filepath.Join(axdir.State("live"), id), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}

	meta := BuildMeta([]session.Session{{ID: id, Parent: "coord"}}, nil, nil)
	m := meta[id]
	if m.State == view.StateLive || m.Activity == view.Working || m.DisplayPhase == view.PhaseLiveWorking {
		t.Fatalf("dead fresh heartbeat rendered live/working: %+v", m)
	}
	if m.Lifecycle != state.LifecycleConcluded || m.DisplayPhase != view.PhaseConcluded {
		t.Fatalf("dead concluded heartbeat phase = lifecycle %q display %q, want concluded", m.Lifecycle, m.DisplayPhase)
	}
}

func writeInvalidLiveRecordForTest(t *testing.T, id string, lastOutput time.Time) {
	t.Helper()
	path := filepath.Join(axdir.State("live"), id)
	data := []byte(fmt.Sprintf("%d\t-1\tax run", lastOutput.Unix()))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
}
