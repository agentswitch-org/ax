//go:build unix

package shell

import "os/exec"

// Command runs cmd through POSIX sh, exactly as ax's `exec.Command("sh", "-c", cmd)`
// call sites did before this package existed.
func Command(cmd string) *exec.Cmd { return exec.Command("sh", "-c", cmd) }

// Prefix is the argv that precedes a command string when it is embedded in
// another program's argument list (e.g. `zellij run -- sh -c <cmd>`).
func Prefix() []string { return []string{"sh", "-c"} }

// ExecReplaceArgs returns the path and argv for exec-replacing the current
// process with a shell running cmd. syscall.Exec does not search PATH, so the
// concrete /bin/sh is used, matching ax's prior behavior.
func ExecReplaceArgs(cmd string) (string, []string) {
	return "/bin/sh", []string{"sh", "-c", cmd}
}

// Quote wraps s as a single POSIX shell literal.
func Quote(s string) string { return posixQuote(s) }

// Invoke renders a program path as a callable token in a shell command string.
// A quoted word is already callable in POSIX sh, so this is just Quote here.
func Invoke(path string) string { return posixInvoke(path) }

// InlineEnv renders KEY=VALUE pairs as a prefix that scopes them to the
// command that follows: the POSIX `K='v' cmd` form.
func InlineEnv(env []string) string { return posixInlineEnv(env) }

// Background returns a command that fires cmd detached: sh runs `cmd &` and
// exits immediately, so the real command is reparented and outlives ax.
func Background(cmd string) *exec.Cmd { return exec.Command("sh", "-c", cmd+" &") }

// InheritEnv renders the ops as `export`/`unset` statements prefixed to cmd and
// run in the same shell, on top of the inherited environment.
func InheritEnv(ops []Op, cmd string) string { return posixInheritEnv(ops, cmd) }

// CleanEnv renders a minimal-environment launch: `env -i` carrying only the ops'
// assignments, then `sh -c '<cmd>'`.
func CleanEnv(ops []Op, cmd string) string { return posixCleanEnv(ops, cmd) }
