package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/session"
)

// TestFileActivityHookPrecedence pins the rule the picker's spinner depends on:
// a fresh hook-reported state wins over the transcript mtime, and only a missing
// or stale hook state falls back to the mtime heuristic.
func TestFileActivityHookPrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	// A transcript last written well outside the working window: the mtime
	// heuristic on its own reads idle.
	tf := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(tf, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(tf, old, old); err != nil {
		t.Fatal(err)
	}
	s := session.Session{ID: "sess-1", File: tf}

	// No hook: falls back to the mtime heuristic -> idle.
	if got := FileActivity(s); got != Idle {
		t.Fatalf("no hook: FileActivity = %q, want %q", got, Idle)
	}

	// Fresh working hook wins over the stale mtime -> working.
	if err := WriteHook(s.ID, "working"); err != nil {
		t.Fatal(err)
	}
	if got := FileActivity(s); got != Working {
		t.Fatalf("fresh working hook: FileActivity = %q, want %q", got, Working)
	}

	// Blocked hook is not working: the spinner reads idle, while needs-you is
	// surfaced separately by Blocked (which is not aged out).
	if err := WriteHook(s.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	if got := FileActivity(s); got != Idle {
		t.Fatalf("blocked hook: FileActivity = %q, want %q", got, Idle)
	}
	if !Blocked(s.ID) {
		t.Fatal("Blocked = false for a blocked hook, want true")
	}

	// A stale working hook (aged past HookFresh) is ignored: activity falls back
	// to the mtime heuristic, so a stuck state never pins the spinner.
	if err := WriteHook(s.ID, "working"); err != nil {
		t.Fatal(err)
	}
	hp := filepath.Join(hookStateDir(), s.ID)
	staleT := time.Now().Add(-HookFresh - time.Minute)
	if err := os.Chtimes(hp, staleT, staleT); err != nil {
		t.Fatal(err)
	}
	if got := FileActivity(s); got != Idle {
		t.Fatalf("stale working hook + old mtime: FileActivity = %q, want %q", got, Idle)
	}

	// Same stale hook, but with a freshly written transcript: the mtime fallback
	// now reads working (the hook no longer masks real activity).
	now := time.Now()
	if err := os.Chtimes(tf, now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(hp, staleT, staleT); err != nil {
		t.Fatal(err)
	}
	if got := FileActivity(s); got != Working {
		t.Fatalf("stale hook + fresh mtime: FileActivity = %q, want %q", got, Working)
	}
}

// TestDoneMarker pins the concluded-worker marker: a "done" hook state reports
// Done (and is not one of the spinner activity values), while other states do not,
// so the picker can show a concluded worker as done rather than a frozen idle.
func TestDoneMarker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	id := "sess-done"

	if Done(id) {
		t.Fatal("no hook: Done = true, want false")
	}
	for _, st := range []string{"working", "idle", "blocked"} {
		if err := WriteHook(id, st); err != nil {
			t.Fatal(err)
		}
		if Done(id) {
			t.Fatalf("%q hook: Done = true, want false", st)
		}
	}
	if err := WriteHook(id, "done"); err != nil {
		t.Fatal(err)
	}
	if !Done(id) {
		t.Fatal("done hook: Done = false, want true")
	}

	// The marker is not aged out (a durable terminal state), unlike the working
	// spinner that falls back to the mtime heuristic once stale.
	hp := filepath.Join(hookStateDir(), id)
	staleT := time.Now().Add(-HookFresh - time.Minute)
	if err := os.Chtimes(hp, staleT, staleT); err != nil {
		t.Fatal(err)
	}
	if !Done(id) {
		t.Fatal("stale done hook: Done = false, want true (not aged out)")
	}

	// ComputeAll surfaces it on the Runtime so the picker and the wire format agree.
	rt := ComputeAll([]session.Session{{ID: id}})
	if !rt[id].Done {
		t.Fatal("ComputeAll did not set Runtime.Done for a done hook")
	}
}

func TestWaitMarkerTracksNonTerminalChildren(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := MarkWaiting("coord", []string{"done-child", "running-child", "running-child"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteHook("done-child", "done"); err != nil {
		t.Fatal(err)
	}
	if !WaitingOnChildren("coord") {
		t.Fatal("coord should be waiting while one marked child is non-terminal")
	}
	rt := ComputeAll([]session.Session{{ID: "coord"}})
	if rt["coord"].Waiting != "" {
		t.Fatalf("non-live owner should not advertise waiting in Runtime, got %q", rt["coord"].Waiting)
	}

	if err := WriteHook("running-child", "failed"); err != nil {
		t.Fatal(err)
	}
	if WaitingOnChildren("coord") {
		t.Fatal("coord should stop waiting once every marked child is terminal")
	}
	ClearWaiting("coord")
	if WaitingOnChildren("coord") {
		t.Fatal("ClearWaiting did not remove the marker")
	}
}

func TestStaleWaitMarkerDoesNotProduceLiveWaiting(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	deadPID := 99999999
	for processAlive(deadPID) {
		deadPID++
	}
	writeWaitMarkerForTest(t, "coord", waitMarker{IDs: []string{"child"}, OwnerPID: deadPID, UpdatedAt: time.Now()})
	if WaitingOnChildren("coord") {
		t.Fatal("dead-owner wait marker should be ignored")
	}
	live.Start("coord", "ax run")
	rt := ComputeAll([]session.Session{{ID: "coord"}})
	if rt["coord"].Waiting != "" {
		t.Fatalf("dead-owner marker produced live-waiting: %q", rt["coord"].Waiting)
	}

	writeWaitMarkerForTest(t, "coord", waitMarker{IDs: []string{"child"}, OwnerPID: os.Getpid(), UpdatedAt: time.Now().Add(-waitMarkerFresh - time.Minute)})
	if WaitingOnChildren("coord") {
		t.Fatal("expired wait marker should be ignored")
	}
}

func TestInvalidLiveProcessDoesNotProduceWorkingRuntime(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := "dead-live"
	tf := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(tf, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(tf, now, now); err != nil {
		t.Fatal(err)
	}
	if err := WriteHook(id, "working"); err != nil {
		t.Fatal(err)
	}
	writeLiveRecordForTest(t, id, now, -1)

	rt := ComputeAll([]session.Session{{ID: id, File: tf}})
	if rt[id].State == Live {
		t.Fatalf("invalid pid heartbeat classified as live: %+v", rt[id])
	}
	if rt[id].Activity == Working {
		t.Fatalf("invalid pid heartbeat classified as working: %+v", rt[id])
	}
	if live.LiveIDs()[id] {
		t.Fatal("invalid pid heartbeat appeared in LiveIDs")
	}
}

func writeWaitMarkerForTest(t *testing.T, owner string, w waitMarker) {
	t.Helper()
	if err := os.MkdirAll(waitStateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(waitStateDir(), owner+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeLiveRecordForTest(t *testing.T, id string, lastOutput time.Time, pid int) {
	t.Helper()
	path := filepath.Join(axdir.State("live"), id)
	data := []byte(fmt.Sprintf("%d\t%d\tax run", lastOutput.Unix(), pid))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
}
