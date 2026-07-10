//go:build windows

package mux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/hold"
	"github.com/agentswitch-org/ax/internal/hold/conpty"
	"github.com/agentswitch-org/ax/internal/shell"
)

// winproc is the Windows process backend: the no-multiplexer twin of the unix
// process backend, built on the machinery the Windows runtime already has
// instead of the POSIX primitives it lacks. The three seams differ, the
// contract does not:
//   - Open spawns the `ax run` holder detached (DETACHED_PROCESS: no console,
//     so it survives the launching ax, its console, and the ssh session; the
//     setsid analog). The holder puts the harness under a ConPTY confined to a
//     kill-on-close job object, so `ax kill` tears the whole tree down.
//   - Send routes through the holder's per-session named pipe as a control
//     connection (hold.SendInput): the pipe already carries INPUT frames for
//     attach clients, so no FIFO analog is needed and input lands on the same
//     ConPTY a viewer's keystrokes do.
//   - Interrupt sends a ctrl-c byte down the same pipe; conhost turns console
//     input 0x03 into the CTRL_C_EVENT a terminal ctrl-c would raise, which is
//     the closest Windows gets to delivering SIGINT to another process.
//
// There are no window methods to honor (same honest no-ops as unix process),
// and no on-disk state of its own: the holder pipe is the rendezvous, derived
// from the session id, and liveness/kill go through the live records the
// holder writes like every other run.
type winproc struct{}

// Active always reports true: selecting this backend means ax manages the
// harness as its own subprocess, so the launcher takes the Open path whether
// or not any terminal is present.
func (winproc) Active() bool     { return true }
func (winproc) HasWindows() bool { return false }

// Open runs cmd (the `ax run` wrapper chain the launcher built) as a detached
// subprocess. title, target, and focus have no meaning without a window and
// are ignored; sessionID needs no side channel here because the holder's named
// pipe (derived from the id) is the input path Send uses.
func (winproc) Open(dir, title, cmd, sessionID, target string, focus bool) error {
	_, _, _, _ = title, sessionID, target, focus
	if cmd == "" {
		return nil
	}
	c := shell.Command(cmd)
	c.Dir = dir
	setDetached(c)
	// The harness gets its terminal from the holder's ConPTY and its input from
	// the holder pipe, so the child's own stdio is detached to NUL.
	if devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		c.Stdin, c.Stdout, c.Stderr = devnull, devnull, devnull
		defer devnull.Close()
	}
	c.Env = os.Environ()
	if err := c.Start(); err != nil {
		// The launcher discards Open errors (a background fan-out must not die on
		// one bad window), so a spawn failure must at least leave a trace.
		axlog.Printf("process backend: spawn for %s failed: %v", sessionID, err)
		return err
	}
	go c.Wait()
	axlog.Printf("process backend: spawned holder for %s (pid %d)", sessionID, c.Process.Pid)
	return nil
}

// Locate has no window to return: correlation is by the holder pipe, which
// Send and Interrupt dial directly.
func (winproc) Locate(string) (string, bool) { return "", false }

// Live has no server-side listing of sessions, so the picker's location column
// is empty for this backend (liveness still shows via the heartbeat records).
func (winproc) Live() map[string]string { return nil }

// Panes has no windows to list.
func (winproc) Panes() []Pane { return nil }

// Focus is a no-op: there is no window to switch to.
func (winproc) Focus(string) error { return nil }

// Send delivers text to the session over the holder's named pipe; enter
// appends a carriage return to submit. Multi-line text is bracketed-paste
// wrapped, as tmux and the unix process backend do, so the harness sees one
// paste instead of submitting at each newline.
//
// The text and the submitting CR go as TWO separate INPUT frames with a
// submitDelay between them, exactly as the tmux and unix process backends
// deliver typed text then a distinct Enter (see submitDelay). A full-screen
// harness TUI (pi, codex) coalesces a text burst and an immediately-following
// CR arriving in ONE write into a single paste event and does NOT submit,
// leaving the prompt sitting unsubmitted in the composer: no turn starts and
// no transcript record appears. Splitting the CR into its own frame after a
// gap makes it register as a deliberate Enter, so `ax send` to an idle
// harness reliably advances a turn instead of silently stranding the prompt.
func (winproc) Send(sessionID, text string, enter bool) error {
	if sessionID == "" {
		return fmt.Errorf("process backend: send needs a session id")
	}
	payload := text
	if strings.Contains(text, "\n") {
		payload = "\x1b[200~" + text + "\x1b[201~"
	}
	if payload != "" {
		if err := hold.SendInput(sessionID, []byte(payload)); err != nil {
			return fmt.Errorf("session %q not open in process backend: %w", sessionID, err)
		}
	}
	if enter {
		if payload != "" {
			time.Sleep(submitDelay) // let a burst-coalescing TUI (pi, codex) settle before submit
		}
		if err := hold.SendInput(sessionID, []byte("\r")); err != nil {
			return fmt.Errorf("session %q not open in process backend: %w", sessionID, err)
		}
	}
	return nil
}

// Interrupt sends a ctrl-c byte through the holder pipe. conhost cooks console
// input, so 0x03 raises CTRL_C_EVENT in the harness's console exactly as a
// terminal ctrl-c would: the turn is redirected without killing the session.
func (winproc) Interrupt(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("process backend: interrupt needs a session id")
	}
	if err := hold.SendInput(sessionID, []byte{0x03}); err != nil {
		return fmt.Errorf("session %q not open in process backend: %w", sessionID, err)
	}
	return nil
}

// PaneTail is empty: a plain subprocess has no terminal scrollback to capture.
func (winproc) PaneTail(string, int) string { return "" }

// MoveWindow is unsupported: a plain subprocess has no window to move.
func (winproc) MoveWindow(sessionID, target string) error {
	return fmt.Errorf("process backend cannot move %s: a subprocess has no window to move into %q", sessionID, target)
}

// CloseWindow is unsupported: a plain subprocess has no window to close.
func (winproc) CloseWindow(sessionID string) error {
	return fmt.Errorf("process backend cannot close a window for %s: a subprocess has no window", sessionID)
}

// Retag is a no-op: there is no per-pane tag to rewrite. A mint-its-own-id
// harness is re-tracked under its real id by `ax run --adopt`, whose holder
// simply also listens on the real id's pipe.
func (winproc) Retag(string) error { return nil }

// processMux is the platform's process (no-multiplexer) backend: on Windows
// the holder-pipe implementation above (the POSIX process backend's FIFO,
// setsid, and SIGINT have no Windows form).
func processMux() Multiplexer { return winproc{} }

// defaultMux is the backend an empty or unknown `mux` setting selects. tmux
// and zellij do not exist on native Windows, so the process backend is the
// default; an explicit "tmux" (an msys/cygwin setup) is still honored.
func defaultMux(name string) Multiplexer {
	if name == "tmux" {
		return tmux{}
	}
	return winproc{}
}

// isProcessMux reports whether the config value resolves to the process
// backend: on Windows everything except an explicitly configured real mux.
func isProcessMux(name string) bool {
	switch name {
	case "tmux", "zellij", "none":
		return false
	}
	return true
}

// setDetached detaches the child from this process's console world AND from a
// launcher-session job (sshd wraps a session's processes in a kill-on-close
// job), the setsid analog. The flag lore lives with the rest of the Windows
// console plumbing: see conpty.DetachedSysProcAttr.
func setDetached(c *exec.Cmd) {
	c.SysProcAttr = conpty.DetachedSysProcAttr()
}

// errProcessBackend marks the POSIX process type's FIFO seams, which never run
// on Windows (backend selection returns winproc instead); they exist so the
// shared process.go compiles.
var errProcessBackend = errors.New("posix process backend not used on Windows")

func openFIFOWrite(path string) (*os.File, error) { return nil, errProcessBackend }

func interruptPID(pid int) error { return errProcessBackend }

func mkFIFO(path string) error { return errProcessBackend }
