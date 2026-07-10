//go:build unix

package mux

import (
	"os"
	"os/exec"
	"syscall"
)

// setDetached makes the child lead its own session and process group (setsid),
// so it survives ax exiting and a group signal cannot travel back up to ax.
func setDetached(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// openFIFOWrite opens the session input FIFO non-blocking for writing, so a send
// to a session whose reader (`ax run`) is gone fails fast instead of blocking on
// a readerless pipe.
func openFIFOWrite(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
}

// interruptPID sends a real SIGINT to the harness pid, cancelling its current
// turn without tearing the session down.
func interruptPID(pid int) error { return syscall.Kill(pid, syscall.SIGINT) }

// mkFIFO creates the session's input FIFO.
func mkFIFO(path string) error { return syscall.Mkfifo(path, 0o600) }

// processMux is the platform's process (no-multiplexer) backend: on unix the
// FIFO/setsid implementation in process.go.
func processMux() Multiplexer { return process{} }

// defaultMux is the backend an empty or unknown `mux` setting selects: tmux,
// so an unset config keeps today's behavior.
func defaultMux(string) Multiplexer { return tmux{} }

// isProcessMux reports whether the config value selects the process backend.
func isProcessMux(name string) bool { return name == "process" }
