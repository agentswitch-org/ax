//go:build windows

package app

import (
	"os"
	"os/exec"

	"github.com/agentswitch-org/ax/internal/finder"
	"github.com/agentswitch-org/ax/internal/hold/conpty"
)

var (
	terminalHandoffForExec = finder.ShutdownInputForExec
	execCommand            = exec.Command
	exitProcess            = os.Exit
)

// execReplace has no execve analog on Windows: it spawns the target, waits for
// it, and exits with the child's status so callers relying on exec-replace
// semantics still terminate this process. It returns an error only when the
// spawn itself fails, so a caller can fall through to its next candidate (e.g.
// dtach missing -> run ax directly).
func execReplace(path string, argv, env []string) error {
	terminalHandoffForExec()
	c := execCommand(path, argv[1:]...)
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		return err
	}
	err := c.Wait()
	exitProcess(exitStatus(err))
	return nil
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}

// setDetached detaches the child from this process's console world AND from a
// launcher-session job (sshd wraps a session's processes in a kill-on-close
// job), the setsid analog. Same attribute as the mux process backend's spawn:
// see conpty.DetachedSysProcAttr for the win01-verified flag lore.
func setDetached(c *exec.Cmd) {
	c.SysProcAttr = conpty.DetachedSysProcAttr()
}

// detachSelf is a no-op on Windows: there are no process groups to leave.
func detachSelf() {}

// processAlive reports whether pid is still running. Windows has no signal-0
// liveness probe; this always reports alive for now (the --close-on-done
// detached closer is a POSIX-only path), so the caller falls back to its
// timeout rather than concluding early.
func processAlive(pid int) bool { return true }
