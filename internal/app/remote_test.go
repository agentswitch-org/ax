package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
)

func writeTestHostConfig(t *testing.T, home, name, transport, shell string) {
	t.Helper()
	cfgDir := filepath.Join(home, "cfg", "ax")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[[host]]\nname = " + strconv.Quote(name) + "\ntransport = " + strconv.Quote(transport) + "\n"
	if shell != "" {
		toml += "shell = " + strconv.Quote(shell) + "\n"
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stubRemoteCommand(t *testing.T, fn remoteCommandFunc) {
	t.Helper()
	orig := runRemoteCommand
	runRemoteCommand = fn
	t.Cleanup(func() { runRemoteCommand = orig })
}

// dehost is the one router every host-taking verb funnels through, so it must
// agree with how Send historically pulled a host out of `--host NAME` and a
// `host/id` first positional, and must not mistake a leftover flag value for the
// routing id.
func TestDehost(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantHost string
		wantRest []string
	}{
		{"explicit --host consumed", []string{"--host", "win01", "abc", "hello"}, "win01", []string{"abc", "hello"}},
		{"host/id qualifier split", []string{"win01/abc", "hello"}, "win01", []string{"abc", "hello"}},
		{"bare local id untouched", []string{"abc", "hello"}, "", []string{"abc", "hello"}},
		{"no args is local", nil, "", []string{}},
		// A value-taking flag left in args must not have its value read as the id.
		// dehost runs after GroupArg, but even raw a `--run R` (no id) is local.
		{"--run R with no id does not route", []string{"--run", "R"}, "", []string{"--run", "R"}},
		// --host wins the host name; the qualifier's id part is still stripped bare,
		// so both forms resolve to one consistent (host, id).
		{"both --host and host/id resolve consistently", []string{"--host", "win01", "win01/abc"}, "win01", []string{"abc"}},
		// Only the FIRST bare positional is split; a later slashed arg (e.g. send
		// text) is forwarded verbatim.
		{"only first positional is split", []string{"win01/abc", "a/b"}, "win01", []string{"abc", "a/b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, rest := dehost(tc.args)
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %#v, want %#v", rest, tc.wantRest)
			}
		})
	}
}

// remoteArgv reruns `<verb> <args...>` on a host. It must strip the pty (-t/-tt),
// quote per the host shell, and inject fail-fast ssh options for one-shot verbs.
func TestRemoteArgv(t *testing.T) {
	t.Run("one-shot ssh: -t stripped, BatchMode+ConnectTimeout injected", func(t *testing.T) {
		h := config.Host{Name: "vm", Transport: "ssh -t vm"}
		prog, argv := remoteArgv(h, "result", []string{"abc"}, false)
		if prog != "ssh" {
			t.Fatalf("prog = %q, want ssh", prog)
		}
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, " -t ") || contains(argv, "-t") {
			t.Fatalf("pty -t not stripped: %v", argv)
		}
		if !contains(argv, "BatchMode=yes") || !contains(argv, "ConnectTimeout=8") {
			t.Fatalf("one-shot ssh missing fail-fast opts: %v", argv)
		}
		// The remote command is `ax result abc`, each arg POSIX-quoted by transportArgv.
		if want := []string{"ax", "'result'", "'abc'"}; !reflect.DeepEqual(argv[len(argv)-3:], want) {
			t.Fatalf("tail = %v, want %v", argv[len(argv)-3:], want)
		}
	})

	t.Run("-tt stripped too", func(t *testing.T) {
		h := config.Host{Name: "vm", Transport: "ssh -tt vm"}
		_, argv := remoteArgv(h, "kill", []string{"abc"}, false)
		if contains(argv, "-tt") || contains(argv, "-t") {
			t.Fatalf("pty not stripped: %v", argv)
		}
	})

	t.Run("streaming ssh: -t stripped, no ssh opts injected", func(t *testing.T) {
		h := config.Host{Name: "vm", Transport: "ssh -t vm"}
		_, argv := remoteArgv(h, "wait", []string{"abc"}, true)
		if contains(argv, "-t") {
			t.Fatalf("pty -t not stripped for streaming: %v", argv)
		}
		if contains(argv, "BatchMode=yes") || contains(argv, "ConnectTimeout=8") {
			t.Fatalf("streaming verb must run unbounded, no BatchMode/ConnectTimeout: %v", argv)
		}
	})

	t.Run("raw_argv: no ssh opts, verbatim args", func(t *testing.T) {
		h := config.Host{Name: "pod", Transport: "kubectl exec pod --", RawArgv: true}
		_, argv := remoteArgv(h, "tag", []string{"x=y'z"}, false)
		if contains(argv, "BatchMode=yes") || contains(argv, "ConnectTimeout=8") {
			t.Fatalf("raw_argv must not get ssh opts: %v", argv)
		}
		if argv[len(argv)-1] != "x=y'z" {
			t.Fatalf("raw_argv altered arg: %q", argv[len(argv)-1])
		}
	})

	t.Run("pwsh host quotes per shell", func(t *testing.T) {
		h := config.Host{Name: "win01", Transport: "ssh -t win01", Shell: "pwsh"}
		_, argv := remoteArgv(h, "send", []string{"abc", "o'brien"}, false)
		last := argv[len(argv)-1]
		if strings.Contains(last, `'\''`) {
			t.Fatalf("pwsh host used POSIX escaping: %q", last)
		}
		if last != "'o''brien'" {
			t.Fatalf("pwsh quoting = %q, want doubled-quote form", last)
		}
	})

	t.Run("user-set ConnectTimeout is not doubled", func(t *testing.T) {
		h := config.Host{Name: "vm", Transport: "ssh -o ConnectTimeout=3 vm"}
		_, argv := remoteArgv(h, "result", []string{"abc"}, false)
		if strings.Count(strings.Join(argv, " "), "ConnectTimeout=") != 1 {
			t.Fatalf("ConnectTimeout doubled: %v", argv)
		}
		if !contains(argv, "BatchMode=yes") {
			t.Fatalf("BatchMode still expected: %v", argv)
		}
	})
}

// lookupHost resolves a configured host by name; Send and every routed verb rely
// on it to turn a --host/host-qualifier into a transport.
func TestLookupHost(t *testing.T) {
	home := isolate(t)
	writeTestHostConfig(t, home, "win01", "ssh -t win01", "pwsh")
	h := lookupHost("win01")
	if h.Name != "win01" || h.Shell != "pwsh" || h.Transport != "ssh -t win01" {
		t.Fatalf("lookupHost returned %#v", h)
	}
}

func TestDefaultRemoteWaitPreservesSuccessAfterAggregatorCancel(t *testing.T) {
	home := isolate(t)
	writeTestHostConfig(t, home, "win01", "fake-transport", "pwsh")

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := false
	stubRemoteCommand(t, func(ctx context.Context, prog string, argv []string) error {
		called = true
		if prog != "fake-transport" {
			t.Fatalf("prog = %q, want fake-transport", prog)
		}
		wantTail := []string{"ax", "'wait'", "'remote-done'", "'--any'"}
		if len(argv) < len(wantTail) || !reflect.DeepEqual(argv[len(argv)-len(wantTail):], wantTail) {
			t.Fatalf("argv tail = %#v, want %#v", argv, wantTail)
		}
		cancel()
		return nil
	})

	if got := defaultRemoteWait(parent, "win01", []string{"remote-done"}, false, 0); got != 0 {
		t.Fatalf("defaultRemoteWait after successful remote completion and aggregate cancel = %d, want 0", got)
	}
	if !called {
		t.Fatal("remote command runner was not called")
	}
}
