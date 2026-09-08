package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/propel"
	"github.com/agentswitch-org/ax/internal/state"
)

func isolatePropel(t *testing.T) string {
	t.Helper()
	rootDir := t.TempDir()
	t.Setenv("HOME", rootDir)
	t.Setenv("USERPROFILE", rootDir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(rootDir, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(rootDir, "cfg"))
	return rootDir
}

func TestNewPropellerPersistsAndRestoresBudget(t *testing.T) {
	rootDir := isolatePropel(t)
	id := "propel-persistence"
	if err := meta.Save(id, meta.Meta{
		Harness: "claude", Task: "fix the parser", Mode: "interactive", KeepLive: true, Dir: rootDir,
		Spec: &meta.Spec{SelfPropel: true, PropelMaxAutoTurns: 1, PropelBackoff: "0s"},
	}); err != nil {
		t.Fatal(err)
	}
	var written strings.Builder
	write := func(b []byte) {
		if meta.Load(id).PropelAutoTurns != 1 {
			t.Fatal("budget was not persisted before input")
		}
		written.Write(b)
	}
	makePump := func() *propel.Propeller {
		return newPropeller(func() string { return id }, write, rootDir, "")
	}
	if got := makePump().OnTurnEnd(); got != propel.ActionReinject {
		t.Fatal(got)
	}
	if !strings.Contains(written.String(), "fix the parser") || !strings.Contains(written.String(), "1/1") {
		t.Fatalf("missing task/budget brief: %q", written.String())
	}
	written.Reset()
	if got := makePump().OnTurnEnd(); got != propel.ActionCapped {
		t.Fatal(got)
	}
	m := meta.Load(id)
	if written.Len() != 0 || !state.Failed(id) || m.Outcome != "failure" || m.Exit == nil || *m.Exit != 1 || !strings.Contains(m.FailReason, "1/1") {
		t.Fatalf("recreated pump did not stop durably: %+v; input=%q", m, written.String())
	}
}

func TestClaudeWatcherRejectsCompletionWhenCheckFailsAtCap(t *testing.T) {
	rootDir := isolatePropel(t)
	id := "00000000-0000-0000-0000-000000000abc"
	transcriptDir := filepath.Join(rootDir, ".claude", "projects", "fixture")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"assistant","message":{"role":"assistant","content":"PROJECT-COMPLETE"},"timestamp":"2026-09-06T00:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(transcriptDir, id+".jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := meta.Save(id, meta.Meta{
		Harness: "claude", Task: "fix it", Mode: "interactive", KeepLive: true, Dir: rootDir,
		PropelAutoTurns: 1,
		Spec:            &meta.Spec{SelfPropel: true, PropelMaxAutoTurns: 1, PropelBackoff: "0s", Accept: "exit 1"},
	}); err != nil {
		t.Fatal(err)
	}
	state.WriteHook(id, state.TurnEnded)
	stop := make(chan struct{})
	timer := time.AfterFunc(5*time.Second, func() { close(stop) })
	defer timer.Stop()
	watchTurnEnd(func() string { return id }, func(b []byte) {
		t.Errorf("exhausted watcher submitted %q", b)
	}, stop)
	m := meta.Load(id)
	if !state.Failed(id) || m.Outcome != "failure" || !strings.Contains(m.FailReason, "1/1") {
		t.Fatalf("watcher treated a failed check as completion: %+v", m)
	}
}
