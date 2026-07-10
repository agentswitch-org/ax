//go:build unix

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// execReplace replaces the current process image with path (execve), used for
// the git/kubectl-style extension dispatch. It returns only on failure.
func execReplace(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}

// watchWinch keeps the harness pty sized to our terminal: it calls resize once
// now and again on every SIGWINCH.
func watchWinch(resize func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			resize()
		}
	}()
	resize()
}

// signalGroup signals the whole process group of pid (negative-pid kill), so a
// configured wrapper shell and the harness it spawned both receive it.
func signalGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

// preparePlainRun gives the no-pty fallback child its own process group, so the
// wrapper can tear down a shell script and its grandchildren as one unit.
func preparePlainRun(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func stopPlainRun(c *exec.Cmd, sig syscall.Signal) {
	if c.Process != nil {
		_ = signalGroup(c.Process.Pid, sig)
	}
}

// cleanupProcessGroup tears down descendants that outlived the direct harness
// child. A shell wrapper can exit while a spawned agent process still holds the
// pty open; without this the wrapper can strand live children after it has
// already reaped the shell.
func cleanupProcessGroup(pid int) {
	if pid <= 0 || !processGroupAlive(pid) {
		return
	}
	_ = signalGroup(pid, syscall.SIGTERM)
	deadline := time.Now().Add(termGrace)
	for time.Now().Before(deadline) {
		if !processGroupAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = signalGroup(pid, syscall.SIGKILL)
}

func processGroupAlive(pid int) bool {
	err := signalGroup(pid, 0)
	return err == nil || err == syscall.EPERM
}

// winchGroup forces a SIGWINCH on the harness's process group: the holder's
// repaint nudge when a reattaching client's size did not change (an unchanged
// TIOCSWINSZ delivers nothing, so a full-screen app would not redraw).
func winchGroup(pid int) { syscall.Kill(-pid, syscall.SIGWINCH) }

// waitSignal reports the signal that terminated the process, if it was signaled.
func waitSignal(c *exec.Cmd) (syscall.Signal, bool) {
	if c.ProcessState == nil {
		return 0, false
	}
	ws, ok := c.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return ws.Signal(), true
}

// makeFIFO creates the process-backend input FIFO.
func makeFIFO(path string) error { return syscall.Mkfifo(path, 0o600) }
