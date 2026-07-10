//go:build unix

package app

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agentswitch-org/ax/internal/hold"
)

type execCalled struct{ argv []string }

// catchExec runs f with execReplaceFn stubbed to record its argv and unwind
// like a real execve (which never returns), so the fall-through run paths are
// not taken, then hands the recorded argv to check.
func catchExec(t *testing.T, f func(), check func(argv []string)) {
	t.Helper()
	orig := execReplaceFn
	execReplaceFn = func(path string, argv, env []string) error {
		panic(execCalled{argv})
	}
	defer func() { execReplaceFn = orig }()
	defer func() {
		c, ok := recover().(execCalled)
		if !ok {
			t.Fatalf("no exec-replace happened")
		}
		check(c.argv)
	}()
	f()
}

// The picker process must never run the native attach client in-process: its
// shared /dev/tty reader lives for the process lifetime and steals the
// client's keystrokes (frozen keys, detach byte never seen). execHeld has to
// exec-replace into a fresh `ax attach --cmd` viewer instead, as the dtach
// backend always did with its dtach exec.
func TestExecHeldNativeExecReplacesIntoFreshAttach(t *testing.T) {
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "none.toml")) // defaults: native holder
	if hold.Backend() != hold.BackendNative {
		t.Fatalf("backend = %q, want native", hold.Backend())
	}
	catchExec(t,
		func() { execHeld("abc", "harness --resume abc") },
		func(argv []string) {
			want := []string{argv[0], "attach", "abc", "--cmd", "harness --resume abc"}
			if !reflect.DeepEqual(argv, want) {
				t.Errorf("execHeld exec argv = %q, want %q", argv, want)
			}
		})
}

func TestExecHeldAdoptNativeExecReplacesIntoFreshAttach(t *testing.T) {
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "none.toml"))
	if hold.Backend() != hold.BackendNative {
		t.Fatalf("backend = %q, want native", hold.Backend())
	}
	catchExec(t,
		func() { execHeldAdopt("ph1", "codex", "codex resume") },
		func(argv []string) {
			want := []string{argv[0], "attach", "ph1", "--adopt", "codex", "--cmd", "codex resume"}
			if !reflect.DeepEqual(argv, want) {
				t.Errorf("execHeldAdopt exec argv = %q, want %q", argv, want)
			}
		})
}
