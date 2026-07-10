package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/retention"
	"github.com/agentswitch-org/ax/internal/state"
)

const codexLifecycleID = "22222222-0000-0000-0000-000000000abc"

func writeCodexLifecycleTranscript(t *testing.T, home, id string, markerAt time.Time) {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "07", "06")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldTurn := markerAt.Add(-2 * time.Second).UTC().Format(time.RFC3339Nano)
	newTurn := markerAt.Add(2 * time.Second).UTC().Format(time.RFC3339Nano)
	content := fmt.Sprintf(`{"type":"session_meta","timestamp":%q,"payload":{"id":%q}}
{"type":"event_msg","timestamp":%q,"payload":{"type":"task_started"}}
{"type":"event_msg","timestamp":%q,"payload":{"type":"turn_aborted"}}
{"type":"event_msg","timestamp":%q,"payload":{"type":"task_started"}}
`, oldTurn, id, oldTurn, oldTurn, newTurn)
	path := filepath.Join(dir, "rollout-2026-07-06T00-00-00-"+id+".jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedStaleCodexFailure(t *testing.T, mode string) {
	t.Helper()
	home := isolate(t)
	markerAt := time.Now().Add(-time.Minute)
	writeCodexLifecycleTranscript(t, home, codexLifecycleID, markerAt)
	exit := 1
	if err := meta.Save(codexLifecycleID, meta.Meta{
		Mode:       mode,
		Harness:    "codex",
		Task:       "ship it",
		KeepLive:   true,
		Outcome:    "failure",
		FailReason: "turn aborted",
		Result:     "old failure",
		Exit:       &exit,
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(codexLifecycleID, "failed"); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(axdir.StatePath("hookstate"), codexLifecycleID)
	if err := os.Chtimes(hook, markerAt, markerAt); err != nil {
		t.Fatal(err)
	}
}

func assertCodexFailureReopened(t *testing.T) {
	t.Helper()
	if state.Terminal(codexLifecycleID) {
		t.Fatal("terminal marker still present after reopen")
	}
	m := meta.Load(codexLifecycleID)
	if m.Outcome != "" || m.FailReason != "" || m.Result != "" || m.Exit != nil {
		t.Fatalf("stale result meta not cleared: outcome=%q reason=%q result=%q exit=%v", m.Outcome, m.FailReason, m.Result, m.Exit)
	}
	if got := resultOutcome(codexLifecycleID, m); got != "pending" {
		t.Fatalf("resultOutcome after reopen = %q, want pending", got)
	}
}

func TestRefreshReopenedTurnClearsStaleFailure(t *testing.T) {
	for _, mode := range []string{"interactive", "headless"} {
		t.Run(mode, func(t *testing.T) {
			seedStaleCodexFailure(t, mode)
			writeFreshLegacyLiveRecord(t, codexLifecycleID)

			if !refreshReopenedTurn(codexLifecycleID) {
				t.Fatal("new turn after failed marker did not reopen lifecycle")
			}
			assertCodexFailureReopened(t)
			if got := waitFor([]string{codexLifecycleID}, true, 10*time.Millisecond, time.Millisecond); got != 124 {
				t.Fatalf("waitFor after reopen = %d, want timeout/pending rather than stale failure", got)
			}
		})
	}
}

func TestRefreshReopenedTurnRequiresVerifiedLiveState(t *testing.T) {
	t.Run("no live record", func(t *testing.T) {
		seedStaleCodexFailure(t, "headless")
		if refreshReopenedTurn(codexLifecycleID) {
			t.Fatal("headless stale marker reopened without a live wrapper")
		}
		if !state.Failed(codexLifecycleID) {
			t.Fatal("failed marker should remain without verified liveness")
		}
	})

	t.Run("wrong start token", func(t *testing.T) {
		seedStaleCodexFailure(t, "headless")
		rec := fmt.Sprintf("%d\t%d\twrong-token\tax run %s", time.Now().Unix(), os.Getpid(), codexLifecycleID)
		if err := os.WriteFile(filepath.Join(axdir.State("live"), codexLifecycleID), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
		if refreshReopenedTurn(codexLifecycleID) {
			t.Fatal("headless stale marker reopened with a mismatched pid start token")
		}
		if !state.Failed(codexLifecycleID) {
			t.Fatal("failed marker should remain when the live record does not verify")
		}
	})
}

func TestLocalReportRefreshesReopenedHeadlessFailure(t *testing.T) {
	seedStaleCodexFailure(t, "headless")
	writeFreshLegacyLiveRecord(t, codexLifecycleID)

	rep := App{}.localReport("", retention.ActiveOnly)
	var found bool
	for _, s := range rep.Sessions {
		if s.ID != codexLifecycleID {
			continue
		}
		found = true
		if s.Failed || s.FailReason != "" || s.Done || s.TerminalAt != (time.Time{}) {
			t.Fatalf("list report kept stale terminal state: failed=%v reason=%q done=%v terminal_at=%v", s.Failed, s.FailReason, s.Done, s.TerminalAt)
		}
		if s.Lifecycle != state.LifecycleLive {
			t.Fatalf("list report lifecycle = %q, want live after stale marker refresh", s.Lifecycle)
		}
	}
	if !found {
		t.Fatalf("codex session missing from local report; got %d sessions", len(rep.Sessions))
	}
	assertCodexFailureReopened(t)
}

func TestConcludeExitRefreshesHeadlessResumeBeforeTerminalNoop(t *testing.T) {
	seedStaleCodexFailure(t, "headless")

	ConcludeExit(codexLifecycleID, 0, "new run finished\n", false)

	if !state.Done(codexLifecycleID) || state.Failed(codexLifecycleID) {
		t.Fatal("headless resumed exit should replace the stale failure with the new success")
	}
	m := meta.Load(codexLifecycleID)
	if m.Outcome != "success" || m.FailReason != "" {
		t.Fatalf("exit outcome/reason = %q/%q, want success with no stale reason", m.Outcome, m.FailReason)
	}
	if m.Exit == nil || *m.Exit != 0 {
		t.Fatalf("exit code = %v, want 0", m.Exit)
	}
}

func writeFreshLegacyLiveRecord(t *testing.T, id string) {
	t.Helper()
	path := filepath.Join(axdir.State("live"), id)
	rec := fmt.Sprintf("%d\tax run legacy", time.Now().Unix())
	if err := os.WriteFile(path, []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}
