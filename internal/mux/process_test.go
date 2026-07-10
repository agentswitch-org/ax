//go:build unix

package mux

import (
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// The methods that have no meaning without a terminal are honest no-ops, so a
// caller sees empty results instead of a false answer.
func TestProcessNoOps(t *testing.T) {
	p := process{}
	if !p.Active() {
		t.Error("Active must be true so the launcher takes the Open path")
	}
	if p.HasWindows() {
		t.Error("HasWindows must be false for the process backend")
	}
	if w, ok := p.Locate("x"); ok || w != "" {
		t.Errorf("Locate = %q,%v, want empty", w, ok)
	}
	if p.Live() != nil {
		t.Error("Live must be nil")
	}
	if p.Panes() != nil {
		t.Error("Panes must be nil")
	}
	if err := p.Focus("x"); err != nil {
		t.Errorf("Focus = %v, want nil", err)
	}
	if s := p.PaneTail("x", 10); s != "" {
		t.Errorf("PaneTail = %q, want empty", s)
	}
	if err := p.Retag("x"); err != nil {
		t.Errorf("Retag = %v, want nil", err)
	}
	// MoveWindow is an error, not a silent no-op, so `ax move` reports the skip.
	if err := p.MoveWindow("x", "target"); err == nil {
		t.Error("MoveWindow must return an error for the no-window backend")
	}
}

// The FIFO and pid paths are a stable function of the session id, so Open (which
// sets AX_PROC_FIFO), `ax run` (which reads it), and Send (which writes it) all
// agree, and distinct ids never collide.
func TestProcessPaths(t *testing.T) {
	if procFIFO("a") == procFIFO("b") || procPIDPath("a") == procPIDPath("b") {
		t.Error("distinct ids must map to distinct files")
	}
	if procFIFO("a") != procFIFO("a") {
		t.Error("path must be stable for the same id")
	}
	if procFIFO("a") == procPIDPath("a") {
		t.Error("FIFO and pid file must not share a path")
	}
}

// ProcTrackPID/procReadPID round-trip the harness pid the wrapper records, and
// ProcClear removes both the pid file and the FIFO on exit.
func TestProcessTrackAndClear(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := "sess-track"
	if err := procMkFIFO(id); err != nil {
		t.Fatal(err)
	}
	if err := ProcTrackPID(id, 4321); err != nil {
		t.Fatal(err)
	}
	if pid, err := procReadPID(id); err != nil || pid != 4321 {
		t.Fatalf("procReadPID = %d,%v, want 4321", pid, err)
	}
	ProcClear(id)
	if _, err := os.Stat(procPIDPath(id)); !os.IsNotExist(err) {
		t.Error("ProcClear must remove the pid file")
	}
	if _, err := os.Stat(procFIFO(id)); !os.IsNotExist(err) {
		t.Error("ProcClear must remove the FIFO")
	}
	if _, err := procReadPID(id); err == nil {
		t.Error("procReadPID must fail after ProcClear")
	}
}

// Send writes into the session FIFO and appends a carriage return when enter is
// set, so `ax run` forwards the exact keystrokes to the harness.
func TestProcessSend(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := "sess-send"
	if err := procMkFIFO(id); err != nil {
		t.Fatal(err)
	}
	// Open O_RDWR (as `ax run` does) so the write side never blocks on a missing
	// reader and the pipe stays open across sends.
	r, err := os.OpenFile(procFIFO(id), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := (process{}).Send(id, "hello", true); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, r, len("hello\r")); got != "hello\r" {
		t.Errorf("Send single-line = %q, want %q", got, "hello\r")
	}

	// Multi-line goes through bracketed paste so the harness sees one paste, not a
	// submit at each newline; no enter means no trailing carriage return.
	if err := (process{}).Send(id, "a\nb", false); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[200~a\nb\x1b[201~"
	if got := readN(t, r, len(want)); got != want {
		t.Errorf("Send multi-line = %q, want %q", got, want)
	}
}

// Send to a session with no reader (its `ax run` is gone) fails fast rather than
// blocking on a readerless pipe.
func TestProcessSendNoReader(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := "sess-noreader"
	if err := procMkFIFO(id); err != nil {
		t.Fatal(err)
	}
	if err := (process{}).Send(id, "hi", true); err == nil {
		t.Error("Send with no reader must return an error, not block")
	}
	// A session that was never opened has no FIFO at all.
	if err := (process{}).Send("never-opened", "hi", true); err == nil {
		t.Error("Send to an unknown session must error")
	}
}

// Interrupt delivers a real SIGINT to the recorded harness pid.
func TestProcessInterrupt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	defer c.Process.Kill()

	id := "sess-int"
	if err := ProcTrackPID(id, c.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := (process{}).Interrupt(id); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("sleep exited %v, want a signal", err)
		}
		ws := ee.Sys().(syscall.WaitStatus)
		if !ws.Signaled() || ws.Signal() != syscall.SIGINT {
			t.Errorf("sleep got signal %v, want SIGINT", ws.Signal())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sleep was not interrupted by SIGINT")
	}
}

// Interrupt on an unknown session (no pid file) is an error, not a nil-pid kill.
func TestProcessInterruptUnknown(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := (process{}).Interrupt("never-opened"); err == nil {
		t.Error("Interrupt on an unknown session must error")
	}
}

// Open creates the input FIFO up front (so a Send that races the child's own
// mkfifo still lands) and starts the command detached.
func TestProcessOpen(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := "sess-open"
	// A non-empty target must be ignored (process has no named session to place a
	// subprocess into): grouping collapses to a flat detached subprocess.
	if err := (process{}).Open(t.TempDir(), "title", "true", id, "ax:proj", false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(procFIFO(id))
	if err != nil {
		t.Fatalf("Open must create the FIFO: %v", err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("Open created %v, want a named pipe", info.Mode())
	}
	// An empty command is a no-op (used by the remote-new path), and never errors.
	if err := (process{}).Open("", "", "", "", "", false); err != nil {
		t.Errorf("Open with empty cmd = %v, want nil", err)
	}
}

func readN(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf)
}
