package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/finder"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
)

func TestArchiveUnarchiveRoundTrip(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000000a01"
	writeClaudeTranscript(t, home, id, "done")

	captureStdout(t, func() { App{}.Archive([]string{id}) })
	if m := meta.Load(id); !m.Archived || m.ArchivedAt.IsZero() {
		t.Fatalf("Archive did not set archived fields: %+v", m)
	}
	captureStdout(t, func() { App{}.Unarchive([]string{id}) })
	if m := meta.Load(id); m.Archived || !m.ArchivedAt.IsZero() {
		t.Fatalf("Unarchive did not clear archived fields: %+v", m)
	}
}

func TestArchiveLiveRequiresForce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	liveIDs := map[string]bool{"live": true}
	if err := archiveSession("live", false, liveIDs); !errors.Is(err, errArchiveLive) {
		t.Fatalf("archiveSession live without force err = %v, want errArchiveLive", err)
	}
	if meta.Load("live").Archived {
		t.Fatal("live session archived without force")
	}
	if err := archiveSession("live", true, liveIDs); err != nil {
		t.Fatal(err)
	}
	if !meta.Load("live").Archived {
		t.Fatal("live session with force was not archived")
	}
}

func TestPickerArchiveChangesUseMetadataAndLiveGuard(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	liveID := "picker-live"
	dormantID := "picker-dormant"
	archivedID := "picker-archived"
	writeLegacyLive(t, liveID, "cmd")
	if err := meta.Save(archivedID, meta.Meta{Archived: true, ArchivedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	errs := App{}.applyArchiveChanges(nil, nil, []finder.ArchiveChange{
		{Session: session.Session{ID: liveID}, Archived: true},
		{Session: session.Session{ID: dormantID}, Archived: true},
		{Session: session.Session{ID: archivedID, Archived: true}, Archived: false},
	})
	if err := errs[liveID]; err != nil {
		t.Fatalf("live archive err = %v, want nil (picker already confirmed)", err)
	}
	if !meta.Load(liveID).Archived {
		t.Fatal("live session was not archived by picker callback despite confirm")
	}
	if !meta.Load(dormantID).Archived {
		t.Fatal("non-live session was not archived by picker callback")
	}
	if m := meta.Load(archivedID); m.Archived || !m.ArchivedAt.IsZero() {
		t.Fatalf("archived session was not restored by picker callback: %+v", m)
	}
}

func TestPruneDryRunScopeAndSafety(t *testing.T) {
	home := isolate(t)
	cfgPath := filepath.Join(home, "retention-test.toml")
	if err := os.WriteFile(cfgPath, []byte("[retention]\nauto_retire = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", cfgPath)
	done := "00000000-0000-0000-0000-000000000b01"
	otherRun := "00000000-0000-0000-0000-000000000b02"
	durable := "00000000-0000-0000-0000-000000000b03"
	liveID := "00000000-0000-0000-0000-000000000b04"
	for _, id := range []string{done, otherRun, durable, liveID} {
		writeClaudeTranscript(t, home, id, "done")
	}
	if err := meta.Save(done, meta.Meta{Parent: "root", Group: "run1"}); err != nil {
		t.Fatal(err)
	}
	if err := meta.Save(otherRun, meta.Meta{Parent: "root", Group: "run2"}); err != nil {
		t.Fatal(err)
	}
	if err := meta.Save(durable, meta.Meta{Group: "run1"}); err != nil {
		t.Fatal(err)
	}
	if err := meta.Save(liveID, meta.Meta{Parent: "root", Group: "run1"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{done, otherRun, durable, liveID} {
		if err := state.WriteHook(id, "done"); err != nil {
			t.Fatal(err)
		}
	}
	writeLegacyLive(t, liveID, "cmd")
	defer live.Remove(liveID)

	out := captureStdout(t, func() { App{}.Prune([]string{"--run", "run1", "--dry-run"}) })
	if !strings.Contains(out, "would archive "+done) || strings.Contains(out, otherRun) {
		t.Fatalf("dry-run output did not respect run scope:\n%s", out)
	}
	if meta.Load(done).Archived {
		t.Fatal("dry-run must not archive")
	}

	out = captureStdout(t, func() { App{}.Prune([]string{"--run", "run1"}) })
	if !strings.Contains(out, "archived "+done) {
		t.Fatalf("prune output missing archived session:\n%s", out)
	}
	if !meta.Load(done).Archived {
		t.Fatal("prune did not archive the concluded ephemeral worker")
	}
	for _, id := range []string{otherRun, durable, liveID} {
		if meta.Load(id).Archived {
			t.Fatalf("%s must not be pruned", id)
		}
	}
}

func TestPruneDryRunWritesNoState(t *testing.T) {
	home := isolate(t)
	cfgPath := filepath.Join(home, "retention-test.toml")
	if err := os.WriteFile(cfgPath, []byte("[retention]\nauto_retire = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", cfgPath)

	id := "00000000-0000-0000-0000-000000000c01"
	writeClaudeTranscript(t, home, id, "done")

	stateRoot := filepath.Join(home, "state")
	before := snapshotStateTree(t, stateRoot)
	out := captureStdout(t, func() { App{}.Prune([]string{"--dry-run"}) })
	if !strings.Contains(out, "dry-run=true") {
		t.Fatalf("dry-run output missing summary:\n%s", out)
	}
	after := snapshotStateTree(t, stateRoot)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("dry-run wrote state files:\nbefore=%v\nafter=%v", before, after)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "ax", "index.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created index cache: %v", err)
	}
}

func TestPruneReapWorkersClosesExistingLingeringWindow(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000001001"
	writeClaudeTranscript(t, home, id, "done")
	if err := meta.Save(id, meta.Meta{Parent: "coord", Group: "run1", Mode: "interactive", Task: "ship it", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	setClaudeTranscriptMTime(t, home, id, time.Now().Add(-time.Hour))
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{windows: map[string]string{id: "ax:run1:1.0"}}

	out := captureStdout(t, func() { App{mux: mx}.Prune([]string{"--run", "run1", "--reap-workers"}) })

	if len(mx.closed) != 1 || mx.closed[0] != id {
		t.Fatalf("closed windows = %v, want [%s]", mx.closed, id)
	}
	if len(*killed) != 1 || (*killed)[0] != id {
		t.Fatalf("reap kill calls = %v, want [%s]", *killed, id)
	}
	if !strings.Contains(out, "reaped worker "+id) || !strings.Contains(out, "lingering mux window") {
		t.Fatalf("prune reap output did not report the lingering window:\n%s", out)
	}
	if got := meta.Load(id); !got.Archived || got.Parent != "coord" || got.Task != "ship it" {
		t.Fatalf("worker reap mutated metadata unexpectedly: %+v", got)
	}
	if !state.Done(id) {
		t.Fatal("worker reap must preserve the terminal hook marker")
	}
}

func TestPruneReapWorkersDryRunReportsWithoutClosing(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000001002"
	writeClaudeTranscript(t, home, id, "done")
	if err := meta.Save(id, meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	setClaudeTranscriptMTime(t, home, id, time.Now().Add(-time.Hour))
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{windows: map[string]string{id: "ax:run1:2.0"}}

	out := captureStdout(t, func() { App{mux: mx}.Prune([]string{"--reap-workers", "--dry-run"}) })

	if len(mx.closed) != 0 {
		t.Fatalf("dry-run closed window(s): %v", mx.closed)
	}
	if len(*killed) != 0 {
		t.Fatalf("dry-run killed session(s): %v", *killed)
	}
	if !strings.Contains(out, "would reap worker "+id) || !strings.Contains(out, "reap_candidates=1") || !strings.Contains(out, "dry-run=true") {
		t.Fatalf("dry-run output did not report the reap target:\n%s", out)
	}
	if !state.Done(id) {
		t.Fatal("dry-run must preserve the terminal hook marker")
	}
}

func TestPruneReapWorkersDryRunCandidatesExpiredKeepLiveDoneResidentWithoutOutcome(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000001006"
	now := time.Now()
	markerAt := now.Add(-30 * time.Minute)
	writeCodexCompletedBeforeTerminalTranscript(t, home, id, markerAt)
	if err := meta.Save(id, meta.Meta{
		Parent:    "coord",
		Group:     "run1",
		Mode:      "interactive",
		Harness:   "codex",
		Task:      "ship it",
		KeepLive:  true,
		KeepUntil: now.Add(-20 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(axdir.StatePath("hookstate"), id), markerAt, markerAt); err != nil {
		t.Fatal(err)
	}
	writeIdleLegacyLive(t, id, now.Add(-time.Hour))
	killed := stubWorkerReapKill(t)

	out := captureStdout(t, func() {
		App{}.Prune([]string{"--run", "run1", "--older-than", "10m", "--reap-workers", "--dry-run", "--all"})
	})

	if len(*killed) != 0 {
		t.Fatalf("dry-run killed session(s): %v", *killed)
	}
	if !strings.Contains(out, "would reap worker "+id) || !strings.Contains(out, "concluded worker with live process") {
		t.Fatalf("expired keep-live done resident worker was not reported as a reap candidate:\n%s", out)
	}
	if !strings.Contains(out, "reap_candidates=1") {
		t.Fatalf("summary should report one reap candidate:\n%s", out)
	}
	if got := meta.Load(id); got.Outcome != "" {
		t.Fatalf("fixture should keep missing outcome, got %q", got.Outcome)
	}
}

func TestPruneReapWorkersDryRunCandidatesLegacyUnparentedTrackedWorker(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000001008"
	now := time.Now()
	writeClaudeTranscript(t, home, id, "done")
	if err := meta.Save(id, meta.Meta{
		Group:     "run1",
		Origin:    "agent",
		Mode:      "interactive",
		Task:      "ship it",
		Labels:    []string{"role=reviewer"},
		KeepLive:  true,
		KeepUntil: now.Add(-20 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	stale := now.Add(-time.Hour)
	setClaudeTranscriptMTime(t, home, id, stale)
	if err := os.Chtimes(filepath.Join(axdir.StatePath("hookstate"), id), stale, stale); err != nil {
		t.Fatal(err)
	}
	writeIdleLegacyLive(t, id, stale)
	killed := stubWorkerReapKill(t)

	out := captureStdout(t, func() {
		App{}.Prune([]string{"--run", "run1", "--older-than", "10m", "--reap-workers", "--dry-run", "--all"})
	})

	if len(*killed) != 0 {
		t.Fatalf("dry-run killed session(s): %v", *killed)
	}
	if !strings.Contains(out, "would reap worker "+id) || !strings.Contains(out, "concluded worker with live process") {
		t.Fatalf("legacy unparented tracked worker was not reported as a reap candidate:\n%s", out)
	}
	if !strings.Contains(out, "reap_candidates=1") {
		t.Fatalf("summary should report one reap candidate:\n%s", out)
	}
}

func TestPruneReapWorkersDryRunCandidatesLegacyHumanOriginTrackedWorkers(t *testing.T) {
	home := isolate(t)
	now := time.Now()
	stale := now.Add(-time.Hour)
	rows := []struct {
		id   string
		role string
		hook string
	}{
		{"00000000-0000-0000-0000-00000000100a", "worker", "done"},
		{"00000000-0000-0000-0000-00000000100b", "reviewer", "failed"},
	}
	windows := map[string]string{}
	for i, row := range rows {
		writeClaudeTranscript(t, home, row.id, row.hook)
		if err := meta.Save(row.id, meta.Meta{
			Group:  "run1",
			Origin: "human",
			Mode:   "interactive",
			Task:   "ship it",
			Labels: []string{"role=" + row.role},
		}); err != nil {
			t.Fatal(err)
		}
		if err := state.WriteHook(row.id, row.hook); err != nil {
			t.Fatal(err)
		}
		setClaudeTranscriptMTime(t, home, row.id, stale)
		if err := os.Chtimes(filepath.Join(axdir.StatePath("hookstate"), row.id), stale, stale); err != nil {
			t.Fatal(err)
		}
		writeIdleLegacyLive(t, row.id, stale)
		windows[row.id] = "ax:run1:human." + strconv.Itoa(i)
	}
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{windows: windows}

	out := captureStdout(t, func() {
		App{mux: mx}.Prune([]string{"--run", "run1", "--older-than", "10m", "--reap-workers", "--dry-run", "--all"})
	})

	if len(mx.closed) != 0 {
		t.Fatalf("dry-run closed window(s): %v", mx.closed)
	}
	if len(*killed) != 0 {
		t.Fatalf("dry-run killed session(s): %v", *killed)
	}
	for _, row := range rows {
		if !strings.Contains(out, "would reap worker "+row.id) {
			t.Fatalf("legacy human-origin %s was not reported as a reap candidate:\n%s", row.role, out)
		}
	}
	if !strings.Contains(out, "concluded worker with live process and mux window") || !strings.Contains(out, "reap_candidates=2") {
		t.Fatalf("dry-run output did not report both resident reap candidates:\n%s", out)
	}
}

func TestPruneReapWorkersSkipsLegacyUnparentedUnsafeSessions(t *testing.T) {
	home := isolate(t)
	currentID := "00000000-0000-0000-0000-00000000110a"
	t.Setenv("AX_SESSION_ID", currentID)
	now := time.Now()
	stale := now.Add(-time.Hour)
	cases := []struct {
		name            string
		id              string
		meta            meta.Meta
		hook            string
		live            bool
		pending         bool
		waiting         bool
		freshTranscript bool
	}{
		{
			name: "human top-level without tracked role",
			id:   "00000000-0000-0000-0000-00000000110b",
			meta: meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it"},
			hook: "done",
		},
		{
			name: "current owner",
			id:   currentID,
			meta: meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=worker"}},
			hook: "done",
		},
		{
			name: "active current worker",
			id:   "00000000-0000-0000-0000-00000000110c",
			meta: meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=worker"}},
			hook: state.Working,
			live: true,
		},
		{
			name: "indefinite keep-live tracked worker",
			id:   "00000000-0000-0000-0000-00000000110d",
			meta: meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=worker"}, KeepLive: true},
			hook: "done",
		},
		{
			name: "active keep-live lease tracked worker",
			id:   "00000000-0000-0000-0000-000000001111",
			meta: meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=worker"}, KeepLive: true, KeepUntil: now.Add(time.Hour)},
			hook: "done",
		},
		{
			name:    "pending ask",
			id:      "00000000-0000-0000-0000-00000000110e",
			meta:    meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=reviewer"}},
			hook:    "done",
			pending: true,
		},
		{
			name:    "child wait",
			id:      "00000000-0000-0000-0000-00000000110f",
			meta:    meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=worker"}},
			hook:    "done",
			waiting: true,
		},
		{
			name:            "fresh transcript activity",
			id:              "00000000-0000-0000-0000-000000001110",
			meta:            meta.Meta{Group: "run1", Origin: "human", Mode: "interactive", Task: "ship it", Labels: []string{"role=worker"}},
			hook:            "done",
			freshTranscript: true,
		},
	}
	windows := map[string]string{}
	for i, tc := range cases {
		writeClaudeTranscript(t, home, tc.id, tc.name)
		if err := meta.Save(tc.id, tc.meta); err != nil {
			t.Fatal(err)
		}
		if err := state.WriteHook(tc.id, tc.hook); err != nil {
			t.Fatal(err)
		}
		if !tc.freshTranscript {
			setClaudeTranscriptMTime(t, home, tc.id, stale)
		}
		if err := os.Chtimes(filepath.Join(axdir.StatePath("hookstate"), tc.id), stale, stale); err != nil {
			t.Fatal(err)
		}
		if tc.live {
			live.Start(tc.id, "ax run")
		}
		if tc.pending {
			if err := ask.Save(tc.id, ask.Pending{Question: "continue?"}); err != nil {
				t.Fatal(err)
			}
		}
		if tc.waiting {
			if err := state.MarkWaiting(tc.id, []string{"00000000-0000-0000-0000-0000000011ff"}); err != nil {
				t.Fatal(err)
			}
		}
		windows[tc.id] = "ax:run1:" + strconv.Itoa(i) + ".0"
	}
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{windows: windows}

	out := captureStdout(t, func() { App{mux: mx}.Prune([]string{"--run", "run1", "--reap-workers", "--dry-run", "--all"}) })

	if len(mx.closed) != 0 {
		t.Fatalf("closed unsafe window(s): %v\noutput:\n%s", mx.closed, out)
	}
	if len(*killed) != 0 {
		t.Fatalf("dry-run killed unsafe session(s): %v\noutput:\n%s", *killed, out)
	}
	if strings.Contains(out, "would reap worker") || !strings.Contains(out, "reap_candidates=0") {
		t.Fatalf("unsafe legacy unparented sessions should not be reap candidates:\n%s", out)
	}
}

func TestPruneReapWorkersSkipsActiveResumedHooklessTurnAfterTerminal(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000001007"
	now := time.Now()
	markerAt := now.Add(-30 * time.Minute)
	writeCodexResumedAfterTerminalTranscript(t, home, id, markerAt)
	if err := meta.Save(id, meta.Meta{
		Parent:    "coord",
		Group:     "run1",
		Mode:      "interactive",
		Harness:   "codex",
		Task:      "ship it",
		KeepLive:  true,
		KeepUntil: now.Add(-20 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(axdir.StatePath("hookstate"), id), markerAt, markerAt); err != nil {
		t.Fatal(err)
	}
	writeIdleLegacyLive(t, id, now.Add(-time.Hour))
	killed := stubWorkerReapKill(t)

	out := captureStdout(t, func() {
		App{}.Prune([]string{"--run", "run1", "--older-than", "10m", "--reap-workers", "--dry-run", "--all"})
	})

	if len(*killed) != 0 {
		t.Fatalf("dry-run killed session(s): %v", *killed)
	}
	if strings.Contains(out, "would reap worker "+id) {
		t.Fatalf("resumed hookless turn must not be a reap candidate:\n%s", out)
	}
	if !strings.Contains(out, "would skip worker "+id) || !strings.Contains(out, "newer hookless turn after terminal marker") {
		t.Fatalf("dry-run output did not explain the hookless resume skip:\n%s", out)
	}
	if !state.Terminal(id) {
		t.Fatal("dry-run prune must not mutate the stale terminal marker")
	}

	out = captureStdout(t, func() {
		App{}.Prune([]string{"--run", "run1", "--older-than", "10m", "--reap-workers", "--all"})
	})
	if len(*killed) != 0 {
		t.Fatalf("resumed hookless turn was reaped: %v\noutput:\n%s", *killed, out)
	}
	if !strings.Contains(out, "reap_candidates=0") {
		t.Fatalf("resumed hookless turn must leave no reap candidates:\n%s", out)
	}
}

func TestPruneReapWorkersSkipsLegacyUnparentedHooklessResume(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		origin string
	}{
		{"origin agent", "00000000-0000-0000-0000-000000001009", "agent"},
		{"origin human", "00000000-0000-0000-0000-00000000100c", "human"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := isolate(t)
			now := time.Now()
			markerAt := now.Add(-30 * time.Minute)
			writeCodexResumedAfterTerminalTranscript(t, home, tc.id, markerAt)
			if err := meta.Save(tc.id, meta.Meta{
				Group:   "run1",
				Origin:  tc.origin,
				Mode:    "interactive",
				Harness: "codex",
				Task:    "ship it",
				Labels:  []string{"role=worker"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := state.WriteHook(tc.id, "done"); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(filepath.Join(axdir.StatePath("hookstate"), tc.id), markerAt, markerAt); err != nil {
				t.Fatal(err)
			}
			writeIdleLegacyLive(t, tc.id, now.Add(-time.Hour))
			killed := stubWorkerReapKill(t)

			out := captureStdout(t, func() {
				App{}.Prune([]string{"--run", "run1", "--older-than", "10m", "--reap-workers", "--dry-run", "--all"})
			})

			if len(*killed) != 0 {
				t.Fatalf("dry-run killed session(s): %v", *killed)
			}
			if strings.Contains(out, "would reap worker "+tc.id) {
				t.Fatalf("legacy unparented hookless resume must not be a reap candidate:\n%s", out)
			}
			if !strings.Contains(out, "would skip worker "+tc.id) || !strings.Contains(out, "newer hookless turn after terminal marker") {
				t.Fatalf("dry-run output did not explain the hookless resume skip:\n%s", out)
			}
		})
	}
}

func TestPruneReapWorkersDryRunSkipsMuxOnlyFreshTerminalHookWithNewerTranscript(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000001005"
	writeClaudeTranscript(t, home, id, "done before late transcript activity")
	if err := meta.Save(id, meta.Meta{Parent: "coord", Group: "run1", Mode: "interactive", Task: "ship it", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	hookAt := now.Add(-state.HookFresh / 2)
	transcriptAt := hookAt.Add(time.Minute)
	if !transcriptAt.Before(now.Add(-live.Active)) {
		t.Fatalf("test fixture transcript mtime must be newer than hook but outside the recent activity window")
	}
	if err := os.Chtimes(filepath.Join(home, "state", "ax", "hookstate", id), hookAt, hookAt); err != nil {
		t.Fatal(err)
	}
	setClaudeTranscriptMTime(t, home, id, transcriptAt)
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{windows: map[string]string{id: "ax:run1:2.05"}}

	out := captureStdout(t, func() { App{mux: mx}.Prune([]string{"--run", "run1", "--reap-workers", "--dry-run"}) })

	if len(mx.closed) != 0 {
		t.Fatalf("dry-run closed mux-only window with newer transcript activity: %v", mx.closed)
	}
	if len(*killed) != 0 {
		t.Fatalf("dry-run killed mux-only session with newer transcript activity: %v", *killed)
	}
	if strings.Contains(out, "would reap worker "+id) {
		t.Fatalf("newer transcript activity must not be a reap candidate:\n%s", out)
	}
	if !strings.Contains(out, "would skip worker "+id) || !strings.Contains(out, "recent or newer transcript/file activity") {
		t.Fatalf("dry-run output did not explain the transcript activity skip:\n%s", out)
	}
	if !strings.Contains(out, "reap_candidates=0") {
		t.Fatalf("mux-only worker with newer transcript activity must not be a reap candidate:\n%s", out)
	}

	out = captureStdout(t, func() { App{mux: mx}.Prune([]string{"--run", "run1", "--reap-workers"}) })
	if len(mx.closed) != 0 {
		t.Fatalf("closed mux-only window with newer transcript activity: %v\noutput:\n%s", mx.closed, out)
	}
	if len(*killed) != 0 {
		t.Fatalf("killed mux-only session with newer transcript activity: %v\noutput:\n%s", *killed, out)
	}
	if strings.Contains(out, "reaped worker "+id) || !strings.Contains(out, "reap_candidates=0") {
		t.Fatalf("newer transcript activity must not be reaped:\n%s", out)
	}
}

func TestPruneReapWorkersDryRunSkipsMuxOnlyRecentTranscriptActivity(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000001003"
	writeClaudeTranscript(t, home, id, "done before manual activity")
	if err := meta.Save(id, meta.Meta{Parent: "coord", Group: "run1", Mode: "interactive", Task: "ship it", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-state.HookFresh - time.Minute)
	if err := os.Chtimes(filepath.Join(home, "state", "ax", "hookstate", id), stale, stale); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(home, ".claude", "projects", "proj", id+".jsonl")
	now := time.Now()
	if err := os.Chtimes(transcript, now, now); err != nil {
		t.Fatal(err)
	}
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{windows: map[string]string{id: "ax:run1:2.1"}}

	out := captureStdout(t, func() { App{mux: mx}.Prune([]string{"--reap-workers", "--dry-run"}) })

	if len(mx.closed) != 0 {
		t.Fatalf("dry-run closed recently active mux-only window(s): %v", mx.closed)
	}
	if len(*killed) != 0 {
		t.Fatalf("dry-run killed recently active mux-only session(s): %v", *killed)
	}
	if !strings.Contains(out, "would skip worker "+id) || !strings.Contains(out, "recent or newer transcript/file activity") {
		t.Fatalf("dry-run output did not explain the recent activity skip:\n%s", out)
	}
	if !strings.Contains(out, "reap_candidates=0") {
		t.Fatalf("recently active mux-only worker must not be a reap candidate:\n%s", out)
	}
}

func TestPruneReapWorkersReapsStaleMuxOnlyNoActivityWindow(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000001004"
	writeClaudeTranscript(t, home, id, "old done worker")
	if err := meta.Save(id, meta.Meta{Parent: "coord", Group: "run1", Mode: "interactive", Task: "ship it", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-state.HookFresh - time.Minute)
	if err := os.Chtimes(filepath.Join(home, "state", "ax", "hookstate", id), stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(home, ".claude", "projects", "proj", id+".jsonl"), stale, stale); err != nil {
		t.Fatal(err)
	}
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{windows: map[string]string{id: "ax:run1:2.2"}}

	out := captureStdout(t, func() { App{mux: mx}.Prune([]string{"--run", "run1", "--reap-workers"}) })

	if len(mx.closed) != 1 || mx.closed[0] != id {
		t.Fatalf("closed windows = %v, want [%s]", mx.closed, id)
	}
	if len(*killed) != 1 || (*killed)[0] != id {
		t.Fatalf("reap kill calls = %v, want [%s]", *killed, id)
	}
	if !strings.Contains(out, "reaped worker "+id) || !strings.Contains(out, "reap_candidates=1") {
		t.Fatalf("stale mux-only worker should remain reapable:\n%s", out)
	}
}

func TestPruneReapWorkersSkipsUnsafeSessions(t *testing.T) {
	home := isolate(t)
	cases := []struct {
		name    string
		id      string
		meta    meta.Meta
		hook    string
		live    bool
		pending bool
		waiting bool
	}{
		{
			name: "live not terminal",
			id:   "00000000-0000-0000-0000-000000001101",
			meta: meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it", Archived: true},
			hook: state.Idle,
			live: true,
		},
		{
			name: "active working",
			id:   "00000000-0000-0000-0000-000000001102",
			meta: meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it", Archived: true},
			hook: state.Working,
			live: true,
		},
		{
			name:    "owner-side wait",
			id:      "00000000-0000-0000-0000-000000001103",
			meta:    meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it", Archived: true},
			hook:    "done",
			live:    true,
			waiting: true,
		},
		{
			name:    "pending ask",
			id:      "00000000-0000-0000-0000-000000001104",
			meta:    meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it", Archived: true},
			hook:    "done",
			pending: true,
		},
		{
			name: "keep live",
			id:   "00000000-0000-0000-0000-000000001105",
			meta: meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it", KeepLive: true, Archived: true},
			hook: "done",
		},
		{
			name: "durable",
			id:   "00000000-0000-0000-0000-000000001106",
			meta: meta.Meta{Mode: "interactive", Task: "ship it", Archived: true},
			hook: "done",
		},
	}
	windows := map[string]string{}
	for i, tc := range cases {
		writeClaudeTranscript(t, home, tc.id, tc.name)
		if err := meta.Save(tc.id, tc.meta); err != nil {
			t.Fatal(err)
		}
		if err := state.WriteHook(tc.id, tc.hook); err != nil {
			t.Fatal(err)
		}
		if tc.live {
			live.Start(tc.id, "ax run")
		}
		if tc.pending {
			if err := ask.Save(tc.id, ask.Pending{Question: "continue?"}); err != nil {
				t.Fatal(err)
			}
		}
		if tc.waiting {
			if err := state.MarkWaiting(tc.id, []string{"00000000-0000-0000-0000-0000000011ff"}); err != nil {
				t.Fatal(err)
			}
		}
		windows[tc.id] = "ax:run1:" + strconv.Itoa(i) + ".0"
	}
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{windows: windows}

	out := captureStdout(t, func() { App{mux: mx}.Prune([]string{"--reap-workers"}) })

	if len(mx.closed) != 0 {
		t.Fatalf("closed unsafe window(s): %v\noutput:\n%s", mx.closed, out)
	}
	if len(*killed) != 0 {
		t.Fatalf("killed unsafe session(s): %v\noutput:\n%s", *killed, out)
	}
	if !strings.Contains(out, "reap_candidates=0") {
		t.Fatalf("summary should show no reap candidates:\n%s", out)
	}
}

func TestPruneReapWorkersAlreadyGoneWindowIsSafe(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-000000001201"
	writeClaudeTranscript(t, home, id, "done")
	if err := meta.Save(id, meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	setClaudeTranscriptMTime(t, home, id, time.Now().Add(-time.Hour))
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{
		windows:  map[string]string{id: "ax:run1:3.0"},
		closeErr: errors.New("no window running worker"),
	}

	out := captureStdout(t, func() { App{mux: mx}.Prune([]string{"--reap-workers"}) })

	if len(mx.closed) != 1 || mx.closed[0] != id {
		t.Fatalf("closed windows = %v, want one best-effort close for %s", mx.closed, id)
	}
	if len(*killed) != 1 || (*killed)[0] != id {
		t.Fatalf("already-gone window blocked process cleanup: got %v, want [%s]", *killed, id)
	}
	if !strings.Contains(out, "reaped worker "+id) || !strings.Contains(out, "failed=0") {
		t.Fatalf("already-gone window should be reported as a safe reap:\n%s", out)
	}
}

func setClaudeTranscriptMTime(t *testing.T, home, id string, when time.Time) {
	t.Helper()
	path := filepath.Join(home, ".claude", "projects", "proj", id+".jsonl")
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func writeCodexCompletedBeforeTerminalTranscript(t *testing.T, home, id string, markerAt time.Time) string {
	t.Helper()
	started := markerAt.Add(-2 * time.Minute)
	completed := markerAt.Add(-time.Minute)
	content := fmt.Sprintf(`{"type":"session_meta","timestamp":%q,"payload":{"id":%q,"cwd":%q}}
{"type":"event_msg","timestamp":%q,"payload":{"type":"task_started"}}
{"type":"event_msg","timestamp":%q,"payload":{"type":"agent_message"}}
{"type":"event_msg","timestamp":%q,"payload":{"type":"task_complete","last_agent_message":"done"}}
`, started.UTC().Format(time.RFC3339Nano), id, filepath.Join(home, "proj"),
		started.UTC().Format(time.RFC3339Nano),
		completed.UTC().Format(time.RFC3339Nano),
		completed.UTC().Format(time.RFC3339Nano))
	return writeCodexRetentionTranscript(t, home, id, completed, content)
}

func writeCodexResumedAfterTerminalTranscript(t *testing.T, home, id string, markerAt time.Time) string {
	t.Helper()
	before := markerAt.Add(-2 * time.Minute)
	after := markerAt.Add(time.Minute)
	content := fmt.Sprintf(`{"type":"session_meta","timestamp":%q,"payload":{"id":%q,"cwd":%q}}
{"type":"event_msg","timestamp":%q,"payload":{"type":"task_started"}}
{"type":"event_msg","timestamp":%q,"payload":{"type":"turn_aborted"}}
{"type":"event_msg","timestamp":%q,"payload":{"type":"task_started"}}
`, before.UTC().Format(time.RFC3339Nano), id, filepath.Join(home, "proj"),
		before.UTC().Format(time.RFC3339Nano),
		before.UTC().Format(time.RFC3339Nano),
		after.UTC().Format(time.RFC3339Nano))
	return writeCodexRetentionTranscript(t, home, id, after, content)
}

func writeCodexRetentionTranscript(t *testing.T, home, id string, mtime time.Time, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "07", "07")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-07-07T00-00-00-"+id+".jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeIdleLegacyLive(t *testing.T, id string, lastOutput time.Time) {
	t.Helper()
	rec := strconv.FormatInt(lastOutput.Unix(), 10) + "\tax run --adopt codex"
	if err := axdir.WriteFileAtomic(filepath.Join(axdir.State("live"), id), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPruneReapWorkersHonorsReapDisabled(t *testing.T) {
	home := isolate(t)
	cfgPath := filepath.Join(home, "retention-test.toml")
	if err := os.WriteFile(cfgPath, []byte("[retention]\nreap_concluded_workers = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", cfgPath)
	id := "00000000-0000-0000-0000-000000001301"
	writeClaudeTranscript(t, home, id, "done")
	if err := meta.Save(id, meta.Meta{Parent: "coord", Mode: "interactive", Task: "ship it", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	killed := stubWorkerReapKill(t)
	mx := &reaperMux{windows: map[string]string{id: "ax:run1:4.0"}}

	out := captureStdout(t, func() { App{mux: mx}.Prune([]string{"--reap-workers"}) })

	if len(mx.closed) != 0 {
		t.Fatalf("closed window despite disabled reap: %v", mx.closed)
	}
	if len(*killed) != 0 {
		t.Fatalf("killed session despite disabled reap: %v", *killed)
	}
	if !strings.Contains(out, "reap_candidates=0") {
		t.Fatalf("summary should show no reap candidates when reap is disabled:\n%s", out)
	}
}

func snapshotStateTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		value := "d " + info.Mode().String() + " " + strconv.FormatInt(info.ModTime().UnixNano(), 10)
		if !d.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value = "f " + info.Mode().String() + " " + strconv.FormatInt(info.ModTime().UnixNano(), 10) + " " + string(data)
		}
		out[filepath.ToSlash(rel)] = value
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatal(err)
	}
	return out
}
