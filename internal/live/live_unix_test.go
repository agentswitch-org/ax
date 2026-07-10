//go:build unix

package live

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRunningRejectsFreshHeartbeatWithUnrelatedLivePID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	c := exec.Command("sleep", "30")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.Process != nil {
			_ = c.Process.Kill()
			_, _ = c.Process.Wait()
		}
	})

	pid := c.Process.Pid
	if pidIsAxRun(pid) {
		t.Fatalf("test helper pid %d unexpectedly looks like an ax run wrapper", pid)
	}
	if Running(Entry{Age: 0, PID: pid}) {
		t.Fatalf("unrelated live pid %d verified as running", pid)
	}

	now := time.Now().Unix()
	if err := os.WriteFile(filepath.Join(dir(), "reused"), []byte(fmt.Sprintf("%d\t%d\tax run", now, pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if LiveIDs()["reused"] {
		t.Fatal("LiveIDs included a fresh heartbeat whose recorded pid belongs to an unrelated process")
	}
}

func TestRunningAcceptsLegacyPIDRecordForSameAxRunWrapper(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	ax := filepath.Join(t.TempDir(), "ax")
	if err := os.WriteFile(ax, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(ax, "run", "legacy-session")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.Process != nil {
			_ = c.Process.Kill()
			_, _ = c.Process.Wait()
		}
	})

	pid := c.Process.Pid
	if !pidIsAxRun(pid) {
		t.Fatalf("test helper pid %d does not look like an ax run wrapper: %q", pid, pidCommand(pid))
	}
	now := time.Now().Unix()
	rec := fmt.Sprintf("%d\t%d\tax run legacy-session", now, pid)
	if err := os.WriteFile(filepath.Join(dir(), "legacy-session"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	if !LiveIDs()["legacy-session"] {
		t.Fatal("LiveIDs did not include a fresh legacy pid heartbeat whose pid is still ax run")
	}
}

func TestRunningAndKillRejectAxRunLikePIDWithWrongStartToken(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	ax := filepath.Join(t.TempDir(), "ax")
	if err := os.WriteFile(ax, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(ax, "run", "new-session")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.Process != nil {
			_ = c.Process.Kill()
			_, _ = c.Process.Wait()
		}
	})

	pid := c.Process.Pid
	if !pidIsAxRun(pid) {
		t.Fatalf("test helper pid %d does not look like an ax run wrapper: %q", pid, pidCommand(pid))
	}
	token := processStartToken(pid)
	if token == "" {
		t.Fatalf("test helper pid %d has no process start token", pid)
	}
	wrongToken := token + "-old"
	if Running(Entry{Age: 0, PID: pid, StartToken: wrongToken}) {
		t.Fatalf("ax-run-like pid %d with the wrong start token verified as running", pid)
	}

	now := time.Now().Unix()
	rec := fmt.Sprintf("%d\t%d\t%s\tax run old-session", now, pid, wrongToken)
	if err := os.WriteFile(filepath.Join(dir(), "old-session"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	if LiveIDs()["old-session"] {
		t.Fatal("LiveIDs included a fresh heartbeat whose pid belongs to a different ax-run-like process")
	}
	if err := Kill("old-session"); err != nil {
		t.Fatal(err)
	}
	if !pidAlive(pid) {
		t.Fatal("Kill terminated the different ax-run-like process")
	}
	if _, ok := Snapshot()["old-session"]; ok {
		t.Fatal("Kill left the mismatched stale heartbeat behind")
	}
}
