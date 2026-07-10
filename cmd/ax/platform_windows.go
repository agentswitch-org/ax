//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// errNoFIFO marks the POSIX FIFO seam (AX_PROC_FIFO) as absent on Windows. It is
// never reached: input injection is fully supported here through the holder's
// per-session named pipe (see mux.winproc.Send / hold.SendInput), so the process
// backend never sets AX_PROC_FIFO and procInput short-circuits before makeFIFO.
var errNoFIFO = errors.New("posix input FIFO not used on Windows; input is injected over the holder named pipe")

// execReplace has no execve analog on Windows: it spawns path, waits, and exits
// with the child's status so the extension-dispatch caller still terminates.
// It returns an error only when the spawn itself fails.
func execReplace(path string, argv, env []string) error {
	c := exec.Command(path, argv[1:]...)
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		return err
	}
	err := c.Wait()
	code := 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	os.Exit(code)
	return nil
}

// waitSignal always reports "not signaled" on Windows: WaitStatus carries no
// terminating-signal information there, only an exit code.
func waitSignal(c *exec.Cmd) (syscall.Signal, bool) { return 0, false }

func preparePlainRun(*exec.Cmd) {}

func stopPlainRun(c *exec.Cmd, sig syscall.Signal) {
	if c.Process != nil {
		_ = c.Process.Signal(sig)
	}
}

func cleanupProcessGroup(int) {}

// makeFIFO has no mkfifo analog on Windows and is never called there: the process
// backend injects input over the holder's named pipe instead of a FIFO, so the
// AX_PROC_FIFO path in procInput never runs. It returns errNoFIFO as a guard.
func makeFIFO(path string) error { return errNoFIFO }
