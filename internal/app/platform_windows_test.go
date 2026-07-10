//go:build windows

package app

import (
	"os"
	"os/exec"
	"testing"
)

type execReplaceExit struct{ code int }

func TestWindowsExecReplaceHandoffBeforeSpawn(t *testing.T) {
	origHandoff := terminalHandoffForExec
	origExecCommand := execCommand
	origExitProcess := exitProcess
	t.Cleanup(func() {
		terminalHandoffForExec = origHandoff
		execCommand = origExecCommand
		exitProcess = origExitProcess
	})

	handoff := false
	terminalHandoffForExec = func() { handoff = true }
	execCommand = func(path string, args ...string) *exec.Cmd {
		if !handoff {
			t.Fatalf("exec command was constructed before terminal handoff")
		}
		return exec.Command(os.Args[0], "-test.run=TestWindowsExecReplaceHelper")
	}
	exitProcess = func(code int) { panic(execReplaceExit{code: code}) }

	defer func() {
		r := recover()
		ex, ok := r.(execReplaceExit)
		if !ok {
			t.Fatalf("execReplace did not exit through exitProcess, recovered %v", r)
		}
		if ex.code != 0 {
			t.Fatalf("execReplace exit code = %d, want 0", ex.code)
		}
	}()

	env := append(os.Environ(), "AX_EXEC_REPLACE_HELPER=1")
	if err := execReplace("ignored", []string{"ignored"}, env); err != nil {
		t.Fatalf("execReplace returned error: %v", err)
	}
	t.Fatalf("execReplace returned without exiting")
}

func TestWindowsExecReplaceHelper(t *testing.T) {
	if os.Getenv("AX_EXEC_REPLACE_HELPER") != "1" {
		return
	}
	os.Exit(0)
}
