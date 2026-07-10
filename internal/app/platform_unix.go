//go:build unix

package app

import (
	"os/exec"
	"syscall"
)

// execReplace replaces the current process image with path (execve), so the
// holder/launcher becomes the exec'd program. It returns only on failure.
func execReplace(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}

// setDetached makes the child lead its own session (setsid) so no group signal
// can reach it.
func setDetached(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// detachSelf moves this process into its own group, the fallback when the
// detached closer could not be spawned.
func detachSelf() { syscall.Setpgid(0, 0) }

// processAlive reports whether pid is still running (signal 0 probes liveness).
func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
