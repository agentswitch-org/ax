//go:build windows

package shell

import "os/exec"

// pwsh resolves the PowerShell executable, preferring PowerShell 7+ (pwsh.exe)
// and falling back to the always-present Windows PowerShell (powershell.exe).
func pwsh() string {
	if p, err := exec.LookPath("pwsh"); err == nil {
		return p
	}
	return "powershell.exe"
}

// Command runs cmd through PowerShell. -NoProfile skips the user's profile so a
// launch is not perturbed by interactive customizations; -EncodedCommand takes
// the base64/UTF-16LE command, the PowerShell analog of `sh -c` but immune to
// the command-line re-tokenization that makes a raw multi-line `-Command` fail
// (a line starting with `--task-file` is misread as the `--` unary operator).
// See encodePwshCommand.
func Command(cmd string) *exec.Cmd {
	return exec.Command(pwsh(), "-NoProfile", "-EncodedCommand", encodePwshCommand(cmd))
}

// Prefix is the argv that precedes a command string when it is embedded in
// another program's argument list. It stays on -Command: its callers embed a
// single-line harness launch (mux pane / ConPTY) or render it as a human-facing
// shell-config string, not the multi-line command that motivates -EncodedCommand.
func Prefix() []string { return []string{pwsh(), "-NoProfile", "-Command"} }

// ExecReplaceArgs returns the path and argv for running cmd through PowerShell.
// Windows has no execve; the app-package execReplace spawns this and waits, so
// the path is the resolved shell and argv[0] repeats it. Uses -EncodedCommand
// for the same multi-line robustness as Command.
func ExecReplaceArgs(cmd string) (string, []string) {
	p := pwsh()
	return p, []string{p, "-NoProfile", "-EncodedCommand", encodePwshCommand(cmd)}
}

// Quote wraps s as a single PowerShell literal.
func Quote(s string) string { return pwshQuote(s) }

// Invoke renders a program path as a callable token in a shell command string.
// In PowerShell a bare quoted string is a value, not a call, so the & call
// operator precedes it.
func Invoke(path string) string { return pwshInvoke(path) }

// InlineEnv renders KEY=VALUE pairs as `$env:K = 'v'; ` statements prefixed to
// the command; the -Command shell exits with the command, so the scope matches
// the POSIX per-command form.
func InlineEnv(env []string) string { return pwshInlineEnv(env) }

// Background returns a command that fires cmd detached via Start-Process, which
// spawns an independent hidden PowerShell that survives ax's exit.
func Background(cmd string) *exec.Cmd {
	p := pwsh()
	return exec.Command(p, "-NoProfile", "-EncodedCommand", encodePwshCommand(pwshBackgroundScript(p, cmd)))
}

// InheritEnv renders the ops as PowerShell statements prefixed to cmd and run in
// the same shell, on top of the inherited environment.
func InheritEnv(ops []Op, cmd string) string { return pwshInheritEnv(ops, cmd) }

// CleanEnv renders a minimal-environment launch, emulating `env -i` with a
// snapshot/wipe/reapply sequence of $env: assignments.
func CleanEnv(ops []Op, cmd string) string { return pwshCleanEnv(ops, cmd) }
