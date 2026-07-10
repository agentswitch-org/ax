//go:build unix

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
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/session"
)

func startAxRunStandIn(t *testing.T, id string) int {
	t.Helper()
	ax := filepath.Join(t.TempDir(), "ax")
	if err := os.WriteFile(ax, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(ax, "run", id)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.Process != nil {
			_ = c.Process.Kill()
			_, _ = c.Process.Wait()
		}
	})
	return c.Process.Pid
}

func writeRunningLiveRecordForTest(t *testing.T, id string, pid int, cmd string) {
	t.Helper()
	token := processStartTokenForTest(t, pid)
	rec := fmt.Sprintf("%d\t%d\t%s\t%s", time.Now().Unix(), pid, token, cmd)
	if err := os.WriteFile(filepath.Join(axdir.State("live"), id), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Remove(id) })

	e, ok := live.Snapshot()[id]
	if !ok {
		t.Fatalf("live snapshot missing test fixture %q", id)
	}
	if !live.Running(e) {
		t.Fatalf("running fixture did not verify as live: %#v", e)
	}
}

func TestPickerSingleLocalRunningWithoutHolderDoesNotDuplicate(t *testing.T) {
	setupPickerAttachState(t)
	const id = "running-unheld"
	pid := startAxRunStandIn(t, id)
	writeRunningLiveRecordForTest(t, id, pid, "claude --resume running-unheld")
	mx := &recordingWindowMux{}
	a := App{mux: mx}

	stderr := captureStderr(t, func() {
		a.act(nomuxChoice(session.Session{ID: id, Harness: "claude"}))
	})

	if len(mx.opens) != 0 {
		t.Fatalf("running session without a holder must not open a duplicate window, got %#v", mx.opens)
	}
	if !strings.Contains(stderr, "session is live but no holder answers") {
		t.Fatalf("stderr = %q, want clear live/unattachable failure", stderr)
	}
}

func TestAttachDirectRunningWithoutHolderDoesNotDuplicate(t *testing.T) {
	home := isolate(t)
	base, err := os.MkdirTemp("/tmp", "axs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	t.Setenv("XDG_RUNTIME_DIR", base)
	const id = "00000000-0000-0000-0000-00000000a742"
	writeClaudeTranscript(t, home, id, "still running")
	pid := startAxRunStandIn(t, id)
	writeRunningLiveRecordForTest(t, id, pid, "claude --resume "+id)

	var execs []string
	exitCode := -1
	origHeld, origExit := execHeldFn, exitFn
	execHeldFn = func(id, cmd string) { execs = append(execs, id+"\x00"+cmd) }
	exitFn = func(code int) { exitCode = code }
	t.Cleanup(func() {
		execHeldFn = origHeld
		exitFn = origExit
	})

	stderr := captureStderr(t, func() {
		App{mux: inactiveMux{}}.Attach([]string{id})
	})

	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if len(execs) != 0 {
		t.Fatalf("direct attach must not exec/spawn a duplicate holder, got %#v", execs)
	}
	if !strings.Contains(stderr, "session is live but no holder answers") {
		t.Fatalf("stderr = %q, want clear live/unattachable failure", stderr)
	}
}
