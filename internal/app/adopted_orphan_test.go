package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
)

func stubAdoptedWrapperScan(t *testing.T, wrappers []adoptedWrapper) {
	t.Helper()
	orig := scanAdoptedWrappersFn
	scanAdoptedWrappersFn = func() []adoptedWrapper {
		return append([]adoptedWrapper(nil), wrappers...)
	}
	t.Cleanup(func() { scanAdoptedWrappersFn = orig })
}

func stubAdoptedWrapperKill(t *testing.T) *[]adoptedWrapper {
	t.Helper()
	var killed []adoptedWrapper
	orig := killAdoptedWrapperFn
	killAdoptedWrapperFn = func(w adoptedWrapper) error {
		killed = append(killed, w)
		return nil
	}
	t.Cleanup(func() { killAdoptedWrapperFn = orig })
	return &killed
}

func saveCompletedWorker(t *testing.T, id string, m meta.Meta, markerAt time.Time) {
	t.Helper()
	if m.Mode == "" {
		m.Mode = "interactive"
	}
	if m.Task == "" {
		m.Task = "ship it"
	}
	if m.Outcome == "" {
		m.Outcome = "success"
	}
	if m.Result == "" {
		m.Result = "FIXED"
	}
	exit := 0
	m.Exit = &exit
	if err := meta.Save(id, m); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	if !markerAt.IsZero() {
		if err := os.Chtimes(filepath.Join(os.Getenv("XDG_STATE_HOME"), "ax", "hookstate", id), markerAt, markerAt); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPruneReapWorkersFindsAdoptedWrapperWithoutIndexedRow(t *testing.T) {
	home := isolate(t)
	now := time.Now()
	launchID := "00000000-0000-0000-0000-00000000ad01"
	id := "00000000-0000-0000-0000-00000000ad02"
	if err := meta.SaveAlias(launchID, id); err != nil {
		t.Fatal(err)
	}
	saveCompletedWorker(t, id, meta.Meta{Parent: "coord", Group: "run1", Harness: "codex"}, now.Add(-time.Hour))
	stubAdoptedWrapperScan(t, []adoptedWrapper{{PID: 78582, PGID: 78582, LaunchID: launchID}})
	killed := stubAdoptedWrapperKill(t)
	workerKilled := stubWorkerReapKill(t)

	cfg, _ := config.Load()
	for _, s := range session.IndexReadOnly(cfg) {
		if s.ID == id {
			t.Fatalf("fixture unexpectedly has indexed row for %s under %s: %+v", id, home, s)
		}
	}
	res := captureStdout(t, func() { App{}.Result([]string{launchID, "--json"}) })
	if !strings.Contains(res, `"result":"FIXED"`) || !strings.Contains(res, `"session":"`+id+`"`) {
		t.Fatalf("result did not resolve through the durable alias/meta path:\n%s", res)
	}

	out := captureStdout(t, func() {
		App{}.Prune([]string{"--run", "run1", "--older-than", "10m", "--reap-workers", "--dry-run", "--all"})
	})

	if len(*killed) != 0 || len(*workerKilled) != 0 {
		t.Fatalf("dry-run killed adopted=%v worker=%v", *killed, *workerKilled)
	}
	if !strings.Contains(out, "would reap worker "+id) || !strings.Contains(out, "concluded worker with adopted wrapper") {
		t.Fatalf("dry-run prune did not report the adopted wrapper orphan:\n%s", out)
	}
}

func TestPruneReapWorkersKillsAdoptedWrapperWithoutIndexedRow(t *testing.T) {
	isolate(t)
	now := time.Now()
	launchID := "00000000-0000-0000-0000-00000000ad11"
	id := "00000000-0000-0000-0000-00000000ad12"
	if err := meta.SaveAlias(launchID, id); err != nil {
		t.Fatal(err)
	}
	saveCompletedWorker(t, id, meta.Meta{Parent: "coord", Group: "run1", Harness: "codex"}, now.Add(-time.Hour))
	stubAdoptedWrapperScan(t, []adoptedWrapper{{PID: 78583, PGID: 78583, LaunchID: launchID}})
	killed := stubAdoptedWrapperKill(t)
	workerKilled := stubWorkerReapKill(t)

	out := captureStdout(t, func() {
		App{}.Prune([]string{"--run", "run1", "--older-than", "10m", "--reap-workers", "--all"})
	})

	if len(*killed) != 1 || (*killed)[0].PID != 78583 {
		t.Fatalf("adopted wrapper kills = %+v, want pid 78583", *killed)
	}
	if len(*workerKilled) != 0 {
		t.Fatalf("live heartbeat kill path should not be used for orphan wrapper: %v", *workerKilled)
	}
	if !strings.Contains(out, "reaped worker "+id) {
		t.Fatalf("prune output did not report reaped orphan:\n%s", out)
	}
	if !state.Done(id) || meta.Load(id).Result != "FIXED" {
		t.Fatal("orphan cleanup must preserve the terminal result")
	}
}

func TestReapWorkerKillsAdoptedWrapperWithoutIndexedRow(t *testing.T) {
	isolate(t)
	launchID := "00000000-0000-0000-0000-00000000ad21"
	id := "00000000-0000-0000-0000-00000000ad22"
	if err := meta.SaveAlias(launchID, id); err != nil {
		t.Fatal(err)
	}
	saveCompletedWorker(t, id, meta.Meta{Parent: "coord", Group: "run1", Harness: "codex"}, time.Now().Add(-time.Hour))
	stubAdoptedWrapperScan(t, []adoptedWrapper{{PID: 78584, PGID: 78584, LaunchID: launchID}})
	killed := stubAdoptedWrapperKill(t)
	workerKilled := stubWorkerReapKill(t)

	App{}.ReapWorker([]string{id, "0s"})

	if len(*killed) != 1 || (*killed)[0].PID != 78584 {
		t.Fatalf("delayed reap adopted kills = %+v, want pid 78584", *killed)
	}
	if len(*workerKilled) != 0 {
		t.Fatalf("delayed reap used live heartbeat kill path for orphan wrapper: %v", *workerKilled)
	}
}

func TestKillResolvesAdoptedWrapperThroughLaunchAlias(t *testing.T) {
	isolate(t)
	launchID := "00000000-0000-0000-0000-00000000ad31"
	id := "00000000-0000-0000-0000-00000000ad32"
	if err := meta.SaveAlias(launchID, id); err != nil {
		t.Fatal(err)
	}
	saveCompletedWorker(t, id, meta.Meta{Parent: "coord", Group: "run1", Harness: "codex"}, time.Now().Add(-time.Hour))
	stubAdoptedWrapperScan(t, []adoptedWrapper{{PID: 78585, PGID: 78585, LaunchID: launchID}})
	killed := stubAdoptedWrapperKill(t)

	App{}.Kill([]string{launchID})

	if len(*killed) != 1 || (*killed)[0].SessionID != id {
		t.Fatalf("kill adopted wrapper calls = %+v, want resolved session %s", *killed, id)
	}
	if !state.Done(id) || meta.Load(id).Outcome != "success" {
		t.Fatal("kill fallback must not regress a completed worker result")
	}
}

func TestAdoptedWrapperOrphanSafetyGate(t *testing.T) {
	isolate(t)
	now := time.Now()
	old := now.Add(-time.Hour)
	cases := []struct {
		name      string
		id        string
		meta      meta.Meta
		pending   bool
		waiting   bool
		hook      string
		candidate bool
	}{
		{"parented worker", "00000000-0000-0000-0000-00000000ae01", meta.Meta{Parent: "coord", Group: "run1"}, false, false, "done", true},
		{"human tracked worker", "00000000-0000-0000-0000-00000000ae02", meta.Meta{Group: "run1", Origin: "human", Labels: []string{"role=worker"}}, false, false, "done", true},
		{"human tracked reviewer", "00000000-0000-0000-0000-00000000ae03", meta.Meta{Group: "run1", Origin: "human", Labels: []string{"role=reviewer"}}, false, false, "done", true},
		{"keep live", "00000000-0000-0000-0000-00000000ae04", meta.Meta{Parent: "coord", Group: "run1", KeepLive: true}, false, false, "done", false},
		{"pending ask", "00000000-0000-0000-0000-00000000ae05", meta.Meta{Parent: "coord", Group: "run1"}, true, false, "done", false},
		{"child wait", "00000000-0000-0000-0000-00000000ae06", meta.Meta{Parent: "coord", Group: "run1"}, false, true, "done", false},
		{"working", "00000000-0000-0000-0000-00000000ae07", meta.Meta{Parent: "coord", Group: "run1"}, false, false, state.Working, false},
		{"human root", "00000000-0000-0000-0000-00000000ae08", meta.Meta{Group: "run1", Origin: "human"}, false, false, "done", false},
	}
	var wrappers []adoptedWrapper
	for i, tc := range cases {
		m := tc.meta
		m.Harness = "codex"
		if tc.hook == "done" {
			saveCompletedWorker(t, tc.id, m, old)
		} else {
			if m.Mode == "" {
				m.Mode = "interactive"
			}
			if m.Task == "" {
				m.Task = "ship it"
			}
			if err := meta.Save(tc.id, m); err != nil {
				t.Fatal(err)
			}
			if err := state.WriteHook(tc.id, tc.hook); err != nil {
				t.Fatal(err)
			}
		}
		if tc.pending {
			if err := ask.Save(tc.id, ask.Pending{Question: "continue?"}); err != nil {
				t.Fatal(err)
			}
		}
		if tc.waiting {
			if err := state.MarkWaiting(tc.id, []string{"00000000-0000-0000-0000-00000000aeff"}); err != nil {
				t.Fatal(err)
			}
		}
		wrappers = append(wrappers, adoptedWrapper{PID: 79000 + i, PGID: 79000 + i, LaunchID: tc.id})
	}
	stubAdoptedWrapperScan(t, wrappers)
	killed := stubAdoptedWrapperKill(t)

	out := captureStdout(t, func() {
		App{}.Prune([]string{"--run", "run1", "--reap-workers", "--dry-run", "--all"})
	})

	if len(*killed) != 0 {
		t.Fatalf("dry-run killed adopted wrappers: %+v", *killed)
	}
	for _, tc := range cases {
		has := strings.Contains(out, "would reap worker "+tc.id)
		if has != tc.candidate {
			t.Fatalf("%s candidate = %v, want %v\noutput:\n%s", tc.name, has, tc.candidate, out)
		}
	}
}
