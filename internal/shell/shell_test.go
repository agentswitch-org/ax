package shell

import "testing"

// The renderers are tag-free (see shell.go), so both the POSIX and PowerShell
// output can be asserted on any host without cross-compiling or running Windows.

func TestPosixQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"plain", "'plain'"},
		{"a b", "'a b'"},
		{"it's", `'it'\''s'`},
		{"a\nb", "'a\nb'"}, // newline (an AX_LABELS value) survives inside single quotes
		{"$HOME", "'$HOME'"},
	}
	for _, c := range cases {
		if got := posixQuote(c.in); got != c.want {
			t.Errorf("posixQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPwshQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"plain", "'plain'"},
		{"a b", "'a b'"},
		{"it's", "'it''s'"},
		{"$env:X `backtick`", "'$env:X `backtick`'"}, // literal inside single quotes
		{"he said 'hi'", "'he said ''hi'''"},
		// Injection payload: an embedded quote must be doubled (not POSIX '\''),
		// so the ; calc ; stays inside the single-quoted literal, not tokenized.
		{"';calc;#", "''';calc;#'"},
		// PowerShell's tokenizer accepts the Unicode single-quote variants as
		// string delimiters, so they must be doubled like the ASCII apostrophe.
		{"it’s done", "'it’’s done'"},
		{"‘smart‚ mix‛", "'‘‘smart‚‚ mix‛‛'"},
	}
	for _, c := range cases {
		if got := pwshQuote(c.in); got != c.want {
			t.Errorf("pwshQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPwshQuoteNeutralizesInjection proves the pwsh command-injection is closed:
// a value carrying an embedded single quote plus a command payload must survive
// as one PowerShell single-quoted literal that decodes back to the exact input,
// so the payload is inert data, never tokenized. It also confirms QuotePwsh is
// the exported path transportArgv uses.
func TestPwshQuoteNeutralizesInjection(t *testing.T) {
	payloads := []string{
		`';calc;#`,
		`x="';calc;#"`,
		`run's name`,
		`a'; rm -rf /; echo '`,
	}
	for _, in := range payloads {
		q := QuotePwsh(in)
		if q != pwshQuote(in) {
			t.Fatalf("QuotePwsh(%q) = %q, want pwshQuote form %q", in, q, pwshQuote(in))
		}
		// A well-formed pwsh single-quoted literal: outer single quotes, and the
		// ONLY interior quotes appear as doubled pairs. Decode it the way pwsh's
		// tokenizer does and require it to equal the input exactly.
		if len(q) < 2 || q[0] != '\'' || q[len(q)-1] != '\'' {
			t.Fatalf("QuotePwsh(%q) = %q: not wrapped in single quotes", in, q)
		}
		body := q[1 : len(q)-1]
		decoded, i := "", 0
		for i < len(body) {
			if body[i] == '\'' {
				// Must be a doubled quote; a lone quote here would end the literal
				// early and expose the rest (the injection).
				if i+1 >= len(body) || body[i+1] != '\'' {
					t.Fatalf("QuotePwsh(%q) = %q: unescaped single quote at %d exposes an injection", in, q, i)
				}
				decoded += "'"
				i += 2
				continue
			}
			decoded += string(body[i])
			i++
		}
		if decoded != in {
			t.Errorf("QuotePwsh(%q) decoded to %q, want %q", in, decoded, in)
		}
	}
}

func TestInheritEnv(t *testing.T) {
	cmd := "ax run --hold id claude"
	cases := []struct {
		name       string
		ops        []Op
		posix, win string
	}{
		{
			name:  "no ops returns cmd unchanged",
			ops:   nil,
			posix: cmd,
			win:   cmd,
		},
		{
			name:  "subscription unsets the key env",
			ops:   []Op{Unset("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")},
			posix: "unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN; " + cmd,
			win:   "Remove-Item Env:ANTHROPIC_API_KEY, Env:ANTHROPIC_AUTH_TOKEN -ErrorAction SilentlyContinue; " + cmd,
		},
		{
			name:  "env:VAR forwards a key by reference then unsets the token",
			ops:   []Op{SetRef("ANTHROPIC_API_KEY", "WORK_KEY"), Unset("ANTHROPIC_AUTH_TOKEN")},
			posix: `export ANTHROPIC_API_KEY="$WORK_KEY"; unset ANTHROPIC_AUTH_TOKEN; ` + cmd,
			win:   "$env:ANTHROPIC_API_KEY = $env:WORK_KEY; Remove-Item Env:ANTHROPIC_AUTH_TOKEN -ErrorAction SilentlyContinue; " + cmd,
		},
		{
			name:  "literal overrides are quoted",
			ops:   []Op{SetLiteral("FOO", "bar"), SetLiteral("BAZ", "a b")},
			posix: "export FOO='bar'; export BAZ='a b'; " + cmd,
			win:   "$env:FOO = 'bar'; $env:BAZ = 'a b'; " + cmd,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := posixInheritEnv(c.ops, cmd); got != c.posix {
				t.Errorf("posix = %q, want %q", got, c.posix)
			}
			if got := pwshInheritEnv(c.ops, cmd); got != c.win {
				t.Errorf("pwsh = %q, want %q", got, c.win)
			}
		})
	}
}

func TestCleanEnv(t *testing.T) {
	cmd := "claude --resume id"
	// keep-by-value + a literal AX var + an api key reference + a quoted override,
	// the ordering cleanEnvCmd builds in launch.go.
	ops := []Op{
		SetRef("PATH", "PATH"),
		SetRef("HOME", "HOME"),
		SetLiteral("AX_SESSION_ID", "abc"),
		SetLiteral("AX_LABELS", "role=worker"),
		SetRef("ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"),
		SetLiteral("FOO", "a b"),
	}

	wantPosix := `env -i PATH="$PATH" HOME="$HOME" AX_SESSION_ID='abc' AX_LABELS='role=worker' ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY" FOO='a b' sh -c 'claude --resume id'`
	if got := posixCleanEnv(ops, cmd); got != wantPosix {
		t.Errorf("posixCleanEnv:\n got  %q\n want %q", got, wantPosix)
	}

	wantWin := "$__axenv0 = $env:PATH; $__axenv1 = $env:HOME; $__axenv2 = $env:ANTHROPIC_API_KEY; " +
		`Get-ChildItem Env: | ForEach-Object { Remove-Item "Env:$($_.Name)" }; ` +
		"$env:PATH = $__axenv0; $env:HOME = $__axenv1; $env:AX_SESSION_ID = 'abc'; " +
		"$env:AX_LABELS = 'role=worker'; $env:ANTHROPIC_API_KEY = $__axenv2; $env:FOO = 'a b'; " +
		cmd
	if got := pwshCleanEnv(ops, cmd); got != wantWin {
		t.Errorf("pwshCleanEnv:\n got  %q\n want %q", got, wantWin)
	}
}

func TestInvoke(t *testing.T) {
	// A held-window command invokes ax by absolute path; POSIX calls a quoted
	// word directly, PowerShell needs the & call operator before it.
	path := "/usr/local/bin/my ax"
	if got, want := posixInvoke(path), "'/usr/local/bin/my ax'"; got != want {
		t.Errorf("posixInvoke = %q, want %q", got, want)
	}
	winPath := `C:\Program Files\ax.exe`
	if got, want := pwshInvoke(winPath), `& 'C:\Program Files\ax.exe'`; got != want {
		t.Errorf("pwshInvoke = %q, want %q", got, want)
	}
}

func TestInlineEnv(t *testing.T) {
	cases := []struct {
		name       string
		env        []string
		posix, win string
	}{
		{
			name:  "empty",
			env:   nil,
			posix: "",
			win:   "",
		},
		{
			name:  "ax control vars, values quoted",
			env:   []string{"AX_SESSION_ID=abc", "AX_LABELS=role=worker\nteam=core"},
			posix: "AX_SESSION_ID='abc' AX_LABELS='role=worker\nteam=core' ",
			win:   "$env:AX_SESSION_ID = 'abc'; $env:AX_LABELS = 'role=worker\nteam=core'; ",
		},
		{
			name:  "value needing quote escapes",
			env:   []string{"MSG=it's a test"},
			posix: `MSG='it'\''s a test' `,
			win:   "$env:MSG = 'it''s a test'; ",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := posixInlineEnv(c.env); got != c.posix {
				t.Errorf("posix = %q, want %q", got, c.posix)
			}
			if got := pwshInlineEnv(c.env); got != c.win {
				t.Errorf("pwsh = %q, want %q", got, c.win)
			}
		})
	}
}

func TestPwshBackgroundScript(t *testing.T) {
	got := pwshBackgroundScript("pwsh.exe", "ax send id 'hi'")
	want := "Start-Process -FilePath 'pwsh.exe' -ArgumentList '-NoProfile','-Command','ax send id ''hi''' -WindowStyle Hidden"
	if got != want {
		t.Errorf("pwshBackgroundScript = %q, want %q", got, want)
	}
}
