//go:build unix

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/live"
)

func TestRunHeartbeatReapsExitedShellAndKillsLingeringGroupChild(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", home+"/state")
	t.Setenv("XDG_CONFIG_HOME", home+"/cfg")

	id := "zombie-regression"
	pidFile := home + "/child.pid"
	cmd := fmt.Sprintf("sleep 30 & echo $! > %s; exit 0", quoteForSh(pidFile))
	done := make(chan int, 1)
	go func() { done <- runHeartbeat([]string{id, cmd}) }()

	var child int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if child > 0 {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("background child pid was not recorded")
	}
	defer syscall.Kill(child, syscall.SIGKILL)

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runHeartbeat exit code = %d, want 0", code)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("runHeartbeat hung after direct child exit; lingering pty child was not cleaned up")
	}
	if !waitProcessGone(child, time.Second) {
		t.Fatalf("background child pid %d still exists after runHeartbeat returned", child)
	}
	if _, ok := live.Snapshot()[id]; ok {
		t.Fatal("live heartbeat was not removed after wrapper exit")
	}
}

func TestPlainRunDoesNotHangWhenBackgroundChildHoldsOutputPipe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", home+"/state")
	t.Setenv("XDG_CONFIG_HOME", home+"/cfg")

	id := "plain-pipe-regression"
	pidFile := home + "/plain-child.pid"
	cmd := fmt.Sprintf("sleep 30 & echo $! > %s; exit 0", quoteForSh(pidFile))
	done := make(chan struct {
		code    int
		stopped bool
	}, 1)
	go func() {
		code, _, stopped := plainRun(id, cmd)
		done <- struct {
			code    int
			stopped bool
		}{code: code, stopped: stopped}
	}()

	child := waitForPIDFile(t, pidFile)
	defer syscall.Kill(child, syscall.SIGKILL)

	select {
	case got := <-done:
		if got.code != 0 {
			t.Fatalf("plainRun exit code = %d, want 0", got.code)
		}
		if got.stopped {
			t.Fatal("plainRun reported an ax-initiated stop")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("plainRun hung after direct child exit; inherited stdout/stderr kept cleanup from running")
	}
	if !waitProcessGone(child, time.Second) {
		t.Fatalf("background child pid %d still exists after plainRun returned", child)
	}
	if _, ok := live.Snapshot()[id]; ok {
		t.Fatal("live heartbeat was not removed after plainRun exit")
	}
}

func quoteForSh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func waitProcessGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return !processExists(pid)
}
