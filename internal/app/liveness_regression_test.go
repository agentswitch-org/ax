package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/meta"
)

const continueDeadPIDHelperEnv = "AX_CONTINUE_DEAD_PID_HELPER"

func TestContinueIgnoresFreshDeadPIDLiveArtifact(t *testing.T) {
	if os.Getenv(continueDeadPIDHelperEnv) == "1" {
		runContinueDeadPIDHelper(t)
		return
	}

	id := "11111111-1111-1111-1111-111111111111"
	isolatedClaudeSession(t, id, "continue-run")
	writeFreshLiveRecord(t, id, -1)

	c := exec.Command(os.Args[0], "-test.run=^TestContinueIgnoresFreshDeadPIDLiveArtifact$")
	c.Env = append(os.Environ(),
		continueDeadPIDHelperEnv+"=1",
		"AX_TEST_CONTINUE_ID="+id,
	)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("continue treated a fresh dead-PID artifact as live: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "live but not reuse-ready") {
		t.Fatalf("continue emitted live-session refusal for a dead-PID artifact:\n%s", out)
	}
}

func runContinueDeadPIDHelper(t *testing.T) {
	t.Helper()
	id := os.Getenv("AX_TEST_CONTINUE_ID")
	if id == "" {
		t.Fatal("AX_TEST_CONTINUE_ID is unset")
	}

	rm := &recordingActiveMux{}
	App{mux: rm}.Continue([]string{id, "new cold task"})

	if len(rm.opens) != 1 {
		t.Fatalf("cold continue should open one resumed window, got %d", len(rm.opens))
	}
	open := rm.opens[0]
	if open.sessionID != id {
		t.Fatalf("continued session id = %q, want %q", open.sessionID, id)
	}
	if !strings.Contains(open.cmd, "--resume") || !strings.Contains(open.cmd, "new cold task") {
		t.Fatalf("continue opened non-resume command: %q", open.cmd)
	}
}

func TestGroupLiveCountIgnoresFreshDeadPIDLiveArtifact(t *testing.T) {
	id := "22222222-2222-2222-2222-222222222222"
	isolatedClaudeSession(t, id, "capacity-run")
	writeFreshLiveRecord(t, id, -1)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := groupLiveCount(cfg, "capacity-run"); got != 0 {
		t.Fatalf("fresh dead-PID artifact consumed worker capacity: got %d live workers, want 0", got)
	}
}

func isolatedClaudeSession(t *testing.T, id, group string) {
	t.Helper()
	home := t.TempDir()
	stateHome := filepath.Join(home, "state")
	configHome := filepath.Join(home, "config")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("AX_CONFIG", filepath.Join(configHome, "missing.toml"))

	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, ".claude", "projects", "-test-work")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"old task"}}`+"\n", id, work)
	if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := meta.Save(id, meta.Meta{Task: "old task", Group: group, Origin: "human", Mode: "interactive", Harness: "claude", Dir: work}); err != nil {
		t.Fatal(err)
	}
}

func writeFreshLiveRecord(t *testing.T, id string, pid int) {
	t.Helper()
	path := filepath.Join(axdir.State("live"), id)
	rec := fmt.Sprintf("%d\t%d\tax run", time.Now().Unix(), pid)
	if err := os.WriteFile(path, []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
}
