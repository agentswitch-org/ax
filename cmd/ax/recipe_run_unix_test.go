//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/runs"
	"github.com/agentswitch-org/ax/internal/state"
)

func TestRunHeartbeatRecipeSuccessConcludesRunAndLogs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("AX_PARENT", "")
	id, group := "recipe-ok", "run-recipe-ok"
	logPath := filepath.Join(t.TempDir(), "recipe.log")
	if err := meta.Save(id, meta.Meta{Harness: "recipe", Mode: "recipe", Task: "/tmp/ok.sh", Group: group, Origin: "human", LogPath: logPath}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_RUN", group)
	t.Setenv("AX_GROUP", group)
	t.Setenv("AX_SESSION_ID", id)
	t.Setenv("AX_DEPTH", "0")
	t.Setenv("AX_MAX_DEPTH", "1")

	code := runHeartbeat([]string{id, "printf 'recipe out\\n'; sh -c 'printf \"child run=%s parent=%s\\n\" \"$AX_RUN\" \"$AX_SESSION_ID\"'; printf 'recipe err\\n' >&2"})
	if code != 0 {
		t.Fatalf("runHeartbeat code = %d, want 0", code)
	}
	if !state.Done(id) || state.Failed(id) {
		t.Fatalf("recipe terminal state done=%v failed=%v", state.Done(id), state.Failed(id))
	}
	if got := meta.Load(id).Outcome; got != "success" {
		t.Fatalf("recipe outcome = %q, want success", got)
	}
	rec, ok := runs.Load(group)
	if !ok {
		t.Fatal("run record was not written")
	}
	if rec.Outcome != runs.Success {
		t.Fatalf("run outcome = %q, want %q", rec.Outcome, runs.Success)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "recipe out") || !strings.Contains(got, "recipe err") ||
		!strings.Contains(got, "child run="+group+" parent="+id) {
		t.Fatalf("recipe log = %q, want stdout and stderr", got)
	}
}

func TestRunHeartbeatRecipeFailureConcludesFailed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("AX_PARENT", "")
	id, group := "recipe-bad", "run-recipe-bad"
	if err := meta.Save(id, meta.Meta{Harness: "recipe", Mode: "recipe", Task: "/tmp/bad.sh", Group: group, Origin: "human"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_RUN", group)
	t.Setenv("AX_GROUP", group)

	code := runHeartbeat([]string{id, "printf 'about to fail\\n'; exit 7"})
	if code != 7 {
		t.Fatalf("runHeartbeat code = %d, want 7", code)
	}
	if state.Done(id) || !state.Failed(id) {
		t.Fatalf("recipe terminal state done=%v failed=%v", state.Done(id), state.Failed(id))
	}
	m := meta.Load(id)
	if m.Outcome != "failure" || !strings.Contains(m.FailReason, "about to fail") {
		t.Fatalf("failed recipe meta = %#v", m)
	}
}

func TestPlainRunStopSignalsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "spawn.sh")
	if err := os.WriteFile(script, []byte("sleep 30 &\necho $! > \"$1\"\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sh", script, pidFile)
	preparePlainRun(c)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	childPID := waitForPIDFile(t, pidFile)
	t.Cleanup(func() {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		if c.Process != nil {
			_ = c.Process.Kill()
		}
	})

	stopPlainRun(c, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shell did not exit after process-group SIGTERM")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); err != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("grandchild pid %d survived process-group stop", childPID)
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pid file %s", path)
	return 0
}
