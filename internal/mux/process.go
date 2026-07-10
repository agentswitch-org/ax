package mux

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/shell"
)

// process is the no-multiplexer backend that runs each session as a plain OS
// subprocess instead of a mux window. It is for POSIX hosts with neither tmux
// nor zellij (Windows is out of scope). There is no window or pane, so the
// methods that only mean something against a terminal are honest no-ops
// (Locate/Live/Panes/Focus/PaneTail) while the ones that can be honored without
// one are real:
//   - Open spawns the command detached in its own session (setsid) so it
//     outlives the launching ax process, and points its input at a per-session
//     FIFO that `ax run` reads (see runHeartbeat), giving Send a place to write.
//   - Send writes into that FIFO; `ax run` forwards it to the harness pty, so
//     text arrives at the harness exactly as typed input would.
//   - Interrupt sends a real SIGINT to the harness pid `ax run` recorded, to
//     redirect a turn without killing the session.
//
// tmux/zellij keep session state in a long-lived server, so a later ax
// invocation queries it. There is no such server here, so the per-session FIFO
// and pid file on disk are how a separate `ax send`/`ax kill` re-finds a running
// session. Paths are derived from the session id and hashed to a short fixed
// name, like the dtach socket in package hold.
type process struct{}

// Active always reports true: selecting this backend means ax manages the
// harness as its own subprocess, so the launcher takes the Open path (not the
// detached/dtach fallback) whether or not any terminal is present.
func (process) Active() bool     { return true }
func (process) HasWindows() bool { return false }

// Open runs cmd as a detached subprocess. title, target, and focus have no
// meaning without a window and are ignored (there is no named session to place a
// subprocess into, so grouping collapses to flat). When sessionID is set it
// creates the input FIFO and passes its path in AX_PROC_FIFO so the `ax run`
// wrapper reads harness input from it.
func (process) Open(dir, title, cmd, sessionID, target string, focus bool) error {
	_, _, _ = title, target, focus // no window to title, group, or focus
	if cmd == "" {
		return nil
	}
	if sessionID != "" {
		// Ensure the FIFO exists before the child starts so a Send that races the
		// wrapper's own mkfifo still has a target. mkfifo here is idempotent.
		if err := procMkFIFO(sessionID); err != nil {
			return err
		}
	}
	c := shell.Command(cmd)
	c.Dir = dir
	// setsid: the child leads its own session and process group, so it survives
	// this ax process exiting and a group signal cannot travel back up to ax.
	setDetached(c)
	// The harness gets its pty and reads its input from the FIFO (via `ax run`),
	// so the child's own stdio is detached to /dev/null. Harness output is
	// dropped here; `ax run` still logs a crash tail to the ax log.
	if devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		c.Stdin, c.Stdout, c.Stderr = devnull, devnull, devnull
		defer devnull.Close()
	}
	c.Env = os.Environ()
	if sessionID != "" {
		c.Env = append(c.Env, "AX_PROC_FIFO="+procFIFO(sessionID))
	}
	if err := c.Start(); err != nil {
		return err
	}
	go c.Wait()
	return nil
}

// Locate has no window to return: correlation is by the FIFO/pid files, which
// Send and Interrupt read directly, not through a window handle.
func (process) Locate(string) (string, bool) { return "", false }

// Live has no server-side listing of sessions to enumerate, so the picker's
// location column is empty for this backend.
func (process) Live() map[string]string { return nil }

// Panes has no windows to list (adoption of hand-started windows is a
// terminal-multiplexer concept).
func (process) Panes() []Pane { return nil }

// Focus is a no-op: there is no window to switch to.
func (process) Focus(string) error { return nil }

// Send writes text into the session's input FIFO; enter appends a carriage
// return to submit. It opens the FIFO non-blocking so a send to a session whose
// reader (`ax run`) is gone fails fast instead of blocking on a readerless pipe.
func (process) Send(sessionID, text string, enter bool) error {
	if sessionID == "" {
		return fmt.Errorf("process backend: send needs a session id")
	}
	f, err := openFIFOWrite(procFIFO(sessionID))
	if err != nil {
		return fmt.Errorf("session %q not open in process backend: %w", sessionID, err)
	}
	defer f.Close()
	payload := text
	if strings.Contains(text, "\n") {
		// Bracketed paste, as tmux does for multi-line, so the harness sees one
		// paste instead of submitting at each newline.
		payload = "\x1b[200~" + text + "\x1b[201~"
	}
	if payload != "" {
		if _, err := f.WriteString(payload); err != nil {
			return err
		}
	}
	if enter {
		if payload != "" {
			time.Sleep(submitDelay) // let a burst-coalescing TUI (codex) settle before submit
		}
		if _, err := f.WriteString("\r"); err != nil {
			return err
		}
	}
	return nil
}

// Interrupt sends a real SIGINT to the harness pid `ax run` recorded, the
// process-backend equivalent of a ctrl-c into a pane: it cancels the current
// turn without tearing the session down.
func (process) Interrupt(sessionID string) error {
	pid, err := procReadPID(sessionID)
	if err != nil {
		return fmt.Errorf("session %q not open in process backend: %w", sessionID, err)
	}
	if err := interruptPID(pid); err != nil {
		return fmt.Errorf("interrupt %s (pid %d): %w", sessionID, pid, err)
	}
	return nil
}

// PaneTail is empty: a plain subprocess has no terminal scrollback to capture.
// A blocked-worker check under this backend must read the harness transcript or
// hook state instead of a pane snapshot.
func (process) PaneTail(string, int) string { return "" }

// MoveWindow is unsupported: a plain subprocess has no window to move into
// another mux session. Returned as an error (not a silent no-op) so `ax move`
// reports the skip instead of claiming success.
func (process) MoveWindow(sessionID, target string) error {
	return fmt.Errorf("process backend cannot move %s: a subprocess has no window to move into %q", sessionID, target)
}

// CloseWindow is unsupported: a plain subprocess has no window to close, and it
// is not dtach-held (the launcher skips the hold layer for this backend), so
// there is nothing to detach. Returned as an error (not a silent no-op) so the
// caller reports the skip instead of implying a detach that did not happen.
func (process) CloseWindow(sessionID string) error {
	return fmt.Errorf("process backend cannot close a window for %s: a subprocess has no window", sessionID)
}

// Retag is a no-op: there is no per-pane tag to rewrite. A mint-its-own-id
// harness is re-tracked under its real id by `ax run --adopt` (which migrates
// the FIFO and pid file), so no retag is needed.
func (process) Retag(string) error { return nil }

// procDir is where the process backend keeps its per-session FIFO and pid files.
func procDir() string { return axdir.State("proc") }

// procKey hashes a session id to a short fixed-length filename stem, like the
// dtach socket, so a long id under a long home path stays well within path
// limits and never carries a stray separator.
func procKey(id string) string {
	sum := sha1.Sum([]byte(id))
	return "ax-" + hex.EncodeToString(sum[:8])
}

func procFIFO(id string) string    { return filepath.Join(procDir(), procKey(id)+".in") }
func procPIDPath(id string) string { return filepath.Join(procDir(), procKey(id)+".pid") }

// procMkFIFO creates the session's input FIFO, tolerating one that already
// exists (Open and `ax run` both ensure it).
func procMkFIFO(id string) error {
	if err := os.MkdirAll(procDir(), 0o700); err != nil {
		return err
	}
	if err := mkFIFO(procFIFO(id)); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

func procReadPID(id string) (int, error) {
	data, err := os.ReadFile(procPIDPath(id))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// ProcFIFO is the per-session input pipe path, shared by Open (which points
// AX_PROC_FIFO at it), the `ax run` wrapper (which reads it), and Send (which
// writes it). Exported for the wrapper in main.
func ProcFIFO(id string) string { return procFIFO(id) }

// ProcTrackPID records the harness pid the `ax run` wrapper started, so a later
// Interrupt (a separate ax process) can signal it.
func ProcTrackPID(id string, pid int) error {
	if err := os.MkdirAll(procDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(procPIDPath(id), []byte(strconv.Itoa(pid)), 0o600)
}

// ProcClear removes a session's pid file and input FIFO when the run exits.
func ProcClear(id string) {
	os.Remove(procPIDPath(id))
	os.Remove(procFIFO(id))
}
