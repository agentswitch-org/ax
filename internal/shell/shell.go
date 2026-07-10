// Package shell centralizes every place ax constructs a command for a host
// shell, so the same call sites emit POSIX sh on unix and PowerShell on Windows
// without the callers branching on GOOS themselves. The exported API lives in
// shell_unix.go / shell_windows.go behind identical signatures:
//
//   - Command / Prefix / ExecReplaceArgs: run a command string through the shell
//     (sh -c ... vs pwsh -NoProfile -Command ...).
//   - Quote: quote one value as a single shell literal (POSIX single quotes with
//     '\” escaping vs PowerShell single quotes with ” escaping).
//   - Background: fire a command detached (sh -c "... &" vs Start-Process).
//   - Invoke: render a program path as a callable token inside a command string
//     (a bare quoted string is a value, not a call, in PowerShell, so it takes
//     the & call operator).
//   - InheritEnv / CleanEnv: render an environment-modifying command prefix from
//     an ordered list of Op (export/unset/env -i vs $env:NAME assignments).
//   - InlineEnv: render KEY=VALUE pairs as a prefix scoping them to one command
//     (POSIX `K='v' cmd` vs PowerShell `$env:K = 'v'; cmd`).
//
// QuotePosix is exported tag-free: a command string sent to a remote host over
// an ssh-style transport is re-parsed by the remote host's shell, which ax
// assumes is POSIX, so remote quoting must not follow the local platform.
//
// The actual string rendering is done by the tag-free posix*/pwsh* helpers in
// this file; the tagged files only pick which one runs and supply the exec.Cmd
// wiring. Keeping the renderers tag-free lets both platforms' output be tested
// on any host (see shell_test.go), and lets the unix path reproduce byte-for-byte
// what ax emitted before this package existed, so routing a call site through
// shell is a no-op on unix.
package shell

import (
	"fmt"
	"strings"
)

// Op is one environment modification applied before a launched command runs:
// an unset of one or more variables, a literal assignment, or an assignment
// whose value is copied from another variable's current value (a reference).
// Callers build an ordered []Op and hand it to InheritEnv or CleanEnv; the
// renderer decides how each Op turns into shell syntax.
type Op struct {
	unset []string // variables to remove (nil for a set op)
	name  string   // variable to assign (empty for an unset op)
	value string   // literal value, or the source variable name when ref is set
	ref   bool     // when true, value names a variable to copy rather than a literal
}

// Unset removes names from the environment before the command runs. Multiple
// names collapse into one statement (POSIX `unset A B`), matching ax's prior
// output.
func Unset(names ...string) Op { return Op{unset: names} }

// SetLiteral assigns name the literal value (quoted for the shell).
func SetLiteral(name, value string) Op { return Op{name: name, value: value} }

// SetRef assigns name the current value of the variable srcVar (POSIX "$srcVar"
// expansion / PowerShell $env:srcVar), so a secret can be forwarded from one
// variable to another without ever appearing literally in the command string.
func SetRef(name, srcVar string) Op { return Op{name: name, value: srcVar, ref: true} }

// --- POSIX sh rendering ---------------------------------------------------

// posixQuote wraps s as a single POSIX shell literal: single quotes with the
// '\” dance to embed a literal single quote. An empty string quotes to ” so it
// survives as a present-but-empty argument.
func posixQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuotePosix quotes s for a POSIX shell regardless of the local platform, for
// command strings an ssh-style transport hands to a remote host's shell.
func QuotePosix(s string) string { return posixQuote(s) }

// QuotePwsh quotes s for a PowerShell remote shell regardless of the local
// platform, for command strings an ssh-style transport hands to a host whose
// remote shell is pwsh. PowerShell does not honor the POSIX '\” embedded-quote
// escape; its only single-quote escape is a doubled quote, so a value carrying
// an embedded quote must be quoted this way, not with QuotePosix.
func QuotePwsh(s string) string { return pwshQuote(s) }

// posixInvoke renders a program path as a callable command-string token: a
// quoted word is already callable in POSIX sh.
func posixInvoke(path string) string { return posixQuote(path) }

// posixInlineEnv renders KEY=VALUE pairs as a `K='v' ` prefix, the POSIX
// per-command environment form; each pair must contain '='.
func posixInlineEnv(env []string) string {
	var b strings.Builder
	for _, a := range env {
		i := strings.IndexByte(a, '=')
		b.WriteString(a[:i])
		b.WriteByte('=')
		b.WriteString(posixQuote(a[i+1:]))
		b.WriteByte(' ')
	}
	return b.String()
}

func posixInheritEnv(ops []Op, cmd string) string {
	var pre []string
	for _, op := range ops {
		pre = append(pre, op.posixStmt())
	}
	if len(pre) == 0 {
		return cmd
	}
	return strings.Join(pre, "; ") + "; " + cmd
}

func (op Op) posixStmt() string {
	if len(op.unset) > 0 {
		return "unset " + strings.Join(op.unset, " ")
	}
	if op.ref {
		return "export " + op.name + `="$` + op.value + `"`
	}
	return "export " + op.name + "=" + posixQuote(op.value)
}

func posixCleanEnv(ops []Op, cmd string) string {
	args := []string{"-i"}
	for _, op := range ops {
		if len(op.unset) > 0 {
			continue // env -i starts empty, so an unset is meaningless here
		}
		args = append(args, op.posixArg())
	}
	return "env " + strings.Join(args, " ") + " sh -c " + posixQuote(cmd)
}

func (op Op) posixArg() string {
	if op.ref {
		return op.name + `="$` + op.value + `"`
	}
	return op.name + "=" + posixQuote(op.value)
}

// --- PowerShell rendering -------------------------------------------------

// pwshQuoteEsc doubles every character PowerShell's tokenizer treats as a
// single quote. Beyond the ASCII apostrophe, the grammar also accepts the
// Unicode left/right/low-9/reversed-9 single quotation marks (U+2018..U+201B)
// as string delimiters, so an unescaped smart quote inside a 'quoted' value
// (agent-authored summaries carry them) would end the string early.
var pwshQuoteEsc = strings.NewReplacer(
	"'", "''",
	"‘", "‘‘",
	"’", "’’",
	"‚", "‚‚",
	"‛", "‛‛",
)

// pwshQuote wraps s as a single PowerShell literal: a single-quoted string, where
// the only escape is a doubled quote character. Inside single quotes PowerShell
// does no interpolation, so $, backtick, and double quotes are all literal and
// need no handling. An empty string quotes to ” so it survives as a present
// argument.
func pwshQuote(s string) string {
	return "'" + pwshQuoteEsc.Replace(s) + "'"
}

func pwshInheritEnv(ops []Op, cmd string) string {
	var pre []string
	for _, op := range ops {
		pre = append(pre, op.pwshStmt())
	}
	if len(pre) == 0 {
		return cmd
	}
	return strings.Join(pre, "; ") + "; " + cmd
}

func (op Op) pwshStmt() string {
	if len(op.unset) > 0 {
		items := make([]string, len(op.unset))
		for i, n := range op.unset {
			items[i] = "Env:" + n
		}
		// -ErrorAction SilentlyContinue: removing an already-absent variable is a
		// no-op, matching `unset`'s tolerance of unset names.
		return "Remove-Item " + strings.Join(items, ", ") + " -ErrorAction SilentlyContinue"
	}
	if op.ref {
		return "$env:" + op.name + " = $env:" + op.value
	}
	return "$env:" + op.name + " = " + pwshQuote(op.value)
}

// pwshCleanEnv emulates `env -i`, which PowerShell lacks: snapshot every
// referenced source value first (before the wipe can clear it), remove every
// inherited environment variable, then apply the ops' assignments and run cmd.
func pwshCleanEnv(ops []Op, cmd string) string {
	var snap, set []string
	tmp := make(map[int]string, len(ops))
	n := 0
	for i, op := range ops {
		if len(op.unset) > 0 || !op.ref {
			continue
		}
		v := fmt.Sprintf("$__axenv%d", n)
		snap = append(snap, v+" = $env:"+op.value)
		tmp[i] = v
		n++
	}
	for i, op := range ops {
		if len(op.unset) > 0 {
			continue
		}
		if op.ref {
			set = append(set, "$env:"+op.name+" = "+tmp[i])
		} else {
			set = append(set, "$env:"+op.name+" = "+pwshQuote(op.value))
		}
	}
	parts := make([]string, 0, len(snap)+len(set)+2)
	parts = append(parts, snap...)
	parts = append(parts, `Get-ChildItem Env: | ForEach-Object { Remove-Item "Env:$($_.Name)" }`)
	parts = append(parts, set...)
	parts = append(parts, cmd)
	return strings.Join(parts, "; ")
}

// pwshInvoke renders a program path as a callable command-string token. In
// PowerShell a bare quoted string is a value, not a call, so the & call
// operator precedes it.
func pwshInvoke(path string) string { return "& " + pwshQuote(path) }

// pwshInlineEnv renders KEY=VALUE pairs as `$env:K = 'v'; ` statements ahead of
// a command. PowerShell has no POSIX-style per-command environment prefix, so
// the assignments persist for the whole -Command invocation, which is the same
// scope: the shell exits when the command does.
func pwshInlineEnv(env []string) string {
	var b strings.Builder
	for _, a := range env {
		i := strings.IndexByte(a, '=')
		b.WriteString("$env:")
		b.WriteString(a[:i])
		b.WriteString(" = ")
		b.WriteString(pwshQuote(a[i+1:]))
		b.WriteString("; ")
	}
	return b.String()
}

// pwshBackgroundScript is the PowerShell that fires cmd detached. PowerShell has
// no `&` backgrounding that outlives its shell, so Start-Process spawns an
// independent, hidden PowerShell that survives ax's exit, the closest analog to
// sh's `cmd &`. shellPath is the resolved pwsh/powershell executable.
func pwshBackgroundScript(shellPath, cmd string) string {
	return "Start-Process -FilePath " + pwshQuote(shellPath) +
		" -ArgumentList '-NoProfile','-Command'," + pwshQuote(cmd) +
		" -WindowStyle Hidden"
}
