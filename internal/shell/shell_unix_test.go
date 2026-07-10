//go:build unix

package shell

import (
	"reflect"
	"testing"
)

// The exported unix API must delegate to the POSIX renderers unchanged (the
// byte-for-byte no-op-on-unix guarantee) and keep the sh -c wiring ax relied on.

func TestUnixExports(t *testing.T) {
	if got, want := Quote("it's a test"), posixQuote("it's a test"); got != want {
		t.Errorf("Quote = %q, want %q", got, want)
	}
	if got := Prefix(); !reflect.DeepEqual(got, []string{"sh", "-c"}) {
		t.Errorf("Prefix = %v, want [sh -c]", got)
	}
	if path, argv := ExecReplaceArgs("echo hi"); path != "/bin/sh" || !reflect.DeepEqual(argv, []string{"sh", "-c", "echo hi"}) {
		t.Errorf("ExecReplaceArgs = %q, %v", path, argv)
	}
	if got, want := Command("echo hi").Args, []string{"sh", "-c", "echo hi"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Command.Args = %v, want %v", got, want)
	}
	if got, want := Background("echo hi").Args, []string{"sh", "-c", "echo hi &"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Background.Args = %v, want %v", got, want)
	}
	// The exported env renderers must match the POSIX helpers exactly.
	ops := []Op{Unset("X"), SetLiteral("FOO", "a b")}
	if got, want := InheritEnv(ops, "cmd"), posixInheritEnv(ops, "cmd"); got != want {
		t.Errorf("InheritEnv = %q, want %q", got, want)
	}
	if got, want := CleanEnv(ops, "cmd"), posixCleanEnv(ops, "cmd"); got != want {
		t.Errorf("CleanEnv = %q, want %q", got, want)
	}
	if got, want := Invoke("/usr/local/bin/ax"), posixInvoke("/usr/local/bin/ax"); got != want {
		t.Errorf("Invoke = %q, want %q", got, want)
	}
	if got, want := InlineEnv([]string{"K=a b"}), posixInlineEnv([]string{"K=a b"}); got != want {
		t.Errorf("InlineEnv = %q, want %q", got, want)
	}
	if got, want := QuotePosix("it's"), posixQuote("it's"); got != want {
		t.Errorf("QuotePosix = %q, want %q", got, want)
	}
}
