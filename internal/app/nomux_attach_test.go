package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/finder"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

// inactiveMux is a multiplexer that is never active, standing in for "no tmux".
// It embeds the interface so only Active needs an implementation; the no-mux
// routing paths never call the others.
type inactiveMux struct{ mux.Multiplexer }

func (inactiveMux) Active() bool     { return false }
func (inactiveMux) HasWindows() bool { return false }

// activeMux reports Active() like a real multiplexer or the process backend, so
// a routing test can reach the mux-active branches. Only Active is implemented:
// the process-backend single-pick path must attach in-terminal and never touch a
// window method, and the embedded nil interface makes any stray call panic loudly
// if that regresses.
type activeMux struct{ mux.Multiplexer }

func (activeMux) Active() bool     { return true }
func (activeMux) HasWindows() bool { return false }

type openCall struct {
	dir, title, cmd, sessionID, target string
	focus                              bool
}

type recordingActiveMux struct {
	mux.Multiplexer
	opens []openCall
}

func (m *recordingActiveMux) Active() bool     { return true }
func (m *recordingActiveMux) HasWindows() bool { return false }

func (m *recordingActiveMux) Open(dir, title, cmd, sessionID, target string, focus bool) error {
	m.opens = append(m.opens, openCall{dir: dir, title: title, cmd: cmd, sessionID: sessionID, target: target, focus: focus})
	return nil
}

type scriptedFinder struct {
	finder.Finder
	chooseItem string
	dir        string
}

func (f scriptedFinder) Choose(string, string, []string, []string) (string, string, error) {
	return f.chooseItem, "", nil
}

func (f scriptedFinder) ChooseDir(string, string, func(string) []string, []string) (string, string, error) {
	return f.dir, "", nil
}

type staticDirs []string

func (d staticDirs) Candidates() []string { return append([]string(nil), d...) }

// writeProcessMuxCfg writes a config selecting the process backend and points
// AX_CONFIG at it, so mux.IsProcess() (which reads the on-disk config) reports
// true for the duration of the test.
func writeProcessMuxCfg(t *testing.T) {
	t.Helper()
	writeAppCfg(t, "mux = \"process\"\n")
}

func writeProcessMuxCfgWithClaudeArgs(t *testing.T) {
	t.Helper()
	writeAppCfg(t, "mux = \"process\"\n\n[[harness]]\nname = \"claude\"\nargs = \"--dangerously-skip-permissions\"\n")
}

func writeAppCfg(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)
}

// A single local pick on the process backend (Windows' default) reports Active()
// but has no window to focus: routing it through mux.Open would only spawn the
// holder detached and leave the terminal at a blank prompt. It must attach in
// this terminal via execHeld instead, exactly as the no-multiplexer path does.
func TestProcessBackendSingleLocalExecsIntoHarness(t *testing.T) {
	writeProcessMuxCfg(t)
	local, remote := captureExec(t)
	a := App{mux: activeMux{}}

	a.act(nomuxChoice(session.Session{ID: "abc", Harness: "claude", Dir: ""}))

	if len(*remote) != 0 {
		t.Fatalf("local session must not route to the remote attach path, got %v", *remote)
	}
	if len(*local) != 1 {
		t.Fatalf("process-backend single local pick must exec into the harness exactly once, got %d", len(*local))
	}
	id, cmd, _ := strings.Cut((*local)[0], "\x00")
	if id != "abc" {
		t.Errorf("execHeld got id %q, want abc", id)
	}
	if !strings.Contains(cmd, "claude --resume 'abc'") {
		t.Errorf("execHeld got resume cmd %q, want the claude resume for abc", cmd)
	}
}

// The process backend reports Active(), but for picker `c`/`C` there is no
// window to focus. A new local session must therefore use the same in-terminal
// held exec path as no-mux attach, not process.Open's detached holder path.
func TestProcessBackendPickerNewExecsIntoHarness(t *testing.T) {
	for _, tc := range []struct {
		name     string
		choice   finder.Choice
		wantArgs bool
	}{
		{name: "new", choice: finder.Choice{New: true}},
		{name: "new args", choice: finder.Choice{NewArgs: true}, wantArgs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeProcessMuxCfgWithClaudeArgs(t)
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chdir(cwd) })
			dir := t.TempDir()
			held, adopt, remoteNew := captureNewExec(t)
			m := &recordingActiveMux{}
			a := App{mux: m, find: scriptedFinder{chooseItem: "claude", dir: dir}, dirs: staticDirs{dir}}

			a.act(tc.choice)

			if len(m.opens) != 0 {
				t.Fatalf("process-backend %s must not call mux.Open, got %v", tc.name, m.opens)
			}
			if len(*adopt) != 0 || len(*remoteNew) != 0 {
				t.Fatalf("local %s must only use execHeld, got adopt=%v remoteNew=%v", tc.name, *adopt, *remoteNew)
			}
			if len(*held) != 1 {
				t.Fatalf("process-backend %s must exec into the new harness exactly once, got %d", tc.name, len(*held))
			}
			id, cmd, _ := strings.Cut((*held)[0], "\x00")
			if id == "" {
				t.Errorf("%s execHeld got empty id", tc.name)
			}
			if !strings.Contains(cmd, "claude --session-id") {
				t.Errorf("%s execHeld got launch cmd %q, want a claude new-session launch", tc.name, cmd)
			}
			if gotArgs := strings.Contains(cmd, "--dangerously-skip-permissions"); gotArgs != tc.wantArgs {
				t.Errorf("%s launch args presence = %v, want %v in %q", tc.name, gotArgs, tc.wantArgs, cmd)
			}
			if got, _ := os.Getwd(); !sameDir(t, got, dir) {
				t.Errorf("%s must chdir before exec, got cwd %q want %q", tc.name, got, dir)
			}
		})
	}
}

// A remote picker `c`/`C` under the process backend must exec the transport in
// this terminal. Routing through mux.Open would spawn it detached, while the
// none backend still prints the command for the user to run.
func TestProcessBackendRemoteNewExecsTransport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		choice   finder.Choice
		wantArgs bool
	}{
		{name: "new", choice: finder.Choice{New: true}},
		{name: "new args", choice: finder.Choice{NewArgs: true}, wantArgs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeProcessMuxCfg(t)
			held, adopt, remoteNew := captureNewExec(t)
			m := &recordingActiveMux{}
			a := App{mux: m, find: scriptedFinder{chooseItem: "box"}}
			tc.choice.Config = config.Config{
				Hosts: []config.Host{{Name: "box", Transport: "ssh -t box", Ax: "ax"}},
			}
			tc.choice.Hosts = []view.HostStatus{
				{Name: "local", State: view.HostLocal},
				{Name: "box", State: view.HostOnline},
			}

			a.act(tc.choice)

			if len(m.opens) != 0 {
				t.Fatalf("process-backend remote %s must not call mux.Open, got %v", tc.name, m.opens)
			}
			if len(*held) != 0 || len(*adopt) != 0 {
				t.Fatalf("remote %s must not use local new exec paths, got held=%v adopt=%v", tc.name, *held, *adopt)
			}
			if len(*remoteNew) != 1 {
				t.Fatalf("process-backend remote %s must exec remote new exactly once, got %d", tc.name, len(*remoteNew))
			}
			cmd := (*remoteNew)[0]
			if !strings.Contains(cmd, "ssh -t box") || !strings.Contains(cmd, "ax new") {
				t.Errorf("remote %s cmd %q, want an ssh transport running `ax new`", tc.name, cmd)
			}
			if gotArgs := strings.Contains(cmd, "--with-args"); gotArgs != tc.wantArgs {
				t.Errorf("remote %s with-args presence = %v, want %v in %q", tc.name, gotArgs, tc.wantArgs, cmd)
			}
		})
	}
}

// captureExec swaps the exec-into-harness actions for recorders so a routing
// test can observe which path fired without exec-replacing the test process,
// restoring the originals on cleanup.
func captureExec(t *testing.T) (local *[]string, remote *[]string) {
	t.Helper()
	var l, r []string
	origHeld, origRemote := execHeldFn, execRemoteAttachFn
	execHeldFn = func(id, cmd string) { l = append(l, id+"\x00"+cmd) }
	execRemoteAttachFn = func(cmd string) { r = append(r, cmd) }
	t.Cleanup(func() { execHeldFn, execRemoteAttachFn = origHeld, origRemote })
	return &l, &r
}

func captureNewExec(t *testing.T) (held *[]string, adopt *[]string, remoteNew *[]string) {
	t.Helper()
	var h, a, r []string
	origHeld, origAdopt, origRemoteNew := execHeldFn, execHeldAdoptFn, execRemoteNewFn
	execHeldFn = func(id, cmd string) { h = append(h, id+"\x00"+cmd) }
	execHeldAdoptFn = func(id, harness, cmd string) { a = append(a, id+"\x00"+harness+"\x00"+cmd) }
	execRemoteNewFn = func(cmd string) { r = append(r, cmd) }
	t.Cleanup(func() {
		execHeldFn, execHeldAdoptFn, execRemoteNewFn = origHeld, origAdopt, origRemoteNew
	})
	return &h, &a, &r
}

func nomuxChoice(picked ...session.Session) finder.Choice {
	return finder.Choice{
		Picked: picked,
		Config: config.Config{Harnesses: []config.Harness{claudeHarness()}},
	}
}

func sameDir(t *testing.T, got, want string) bool {
	t.Helper()
	g, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	return g == w
}

// A single pick with no multiplexer must drop the user into the live local
// harness (the execHeld/attach path), not print the resume command.
func TestNoMuxSingleLocalExecsIntoHarness(t *testing.T) {
	local, remote := captureExec(t)
	a := App{mux: inactiveMux{}}

	a.act(nomuxChoice(session.Session{ID: "abc", Harness: "claude", Dir: ""}))

	if len(*remote) != 0 {
		t.Fatalf("local session must not route to the remote attach path, got %v", *remote)
	}
	if len(*local) != 1 {
		t.Fatalf("single local no-mux pick must exec into the harness exactly once, got %d", len(*local))
	}
	id, cmd, _ := strings.Cut((*local)[0], "\x00")
	if id != "abc" {
		t.Errorf("execHeld got id %q, want abc", id)
	}
	if !strings.Contains(cmd, "claude --resume 'abc'") {
		t.Errorf("execHeld got resume cmd %q, want the claude resume for abc", cmd)
	}
}

// A single pick of a remote session with no multiplexer must exec the remote
// attach command over its host's transport.
func TestNoMuxSingleRemoteExecsRemoteAttach(t *testing.T) {
	local, remote := captureExec(t)
	a := App{mux: inactiveMux{}}

	c := nomuxChoice(session.Session{ID: "r1", Harness: "claude", Host: "box"})
	c.Config.Hosts = []config.Host{{Name: "box", Transport: "ssh -t box", Ax: "ax"}}
	a.act(c)

	if len(*local) != 0 {
		t.Fatalf("remote session must not route to the local exec path, got %v", *local)
	}
	if len(*remote) != 1 {
		t.Fatalf("single remote no-mux pick must exec the remote attach exactly once, got %d", len(*remote))
	}
	if got := (*remote)[0]; !strings.Contains(got, "ssh -t box") || !strings.Contains(got, "ax attach") {
		t.Errorf("remote attach cmd %q, want an ssh transport running `ax attach`", got)
	}
}

// Multiple picks with no multiplexer cannot exec into several terminals at once,
// so they keep the old behavior: print each resume command, exec nothing.
func TestNoMuxMultiStillPrints(t *testing.T) {
	local, remote := captureExec(t)
	a := App{mux: inactiveMux{}}

	out := captureStdout(t, func() {
		a.act(nomuxChoice(
			session.Session{ID: "one", Harness: "claude"},
			session.Session{ID: "two", Harness: "claude"},
		))
	})

	if len(*local) != 0 || len(*remote) != 0 {
		t.Fatalf("multi no-mux pick must exec nothing, got local=%v remote=%v", *local, *remote)
	}
	if !strings.Contains(out, "claude --resume 'one'") || !strings.Contains(out, "claude --resume 'two'") {
		t.Errorf("multi no-mux pick must print both resume commands, got:\n%s", out)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote, so a print-path test can assert on the emitted commands.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	w.Close()
	os.Stdout = orig
	return <-done
}
