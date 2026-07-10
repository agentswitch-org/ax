package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/hold"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/session"
)

type recordingWindowMux struct {
	mux.Multiplexer
	loc      map[string]string
	opens    []openCall
	focuses  []string
	openErr  error
	focusErr error
}

func (m *recordingWindowMux) Active() bool     { return true }
func (m *recordingWindowMux) HasWindows() bool { return true }

func (m *recordingWindowMux) Locate(id string) (string, bool) {
	win, ok := m.loc[id]
	return win, ok
}

func (m *recordingWindowMux) Live() map[string]string { return nil }

func (m *recordingWindowMux) Panes() []mux.Pane { return nil }

func (m *recordingWindowMux) Focus(win string) error {
	m.focuses = append(m.focuses, win)
	return m.focusErr
}

func (m *recordingWindowMux) Open(dir, title, cmd, sessionID, target string, focus bool) error {
	m.opens = append(m.opens, openCall{dir: dir, title: title, cmd: cmd, sessionID: sessionID, target: target, focus: focus})
	return m.openErr
}

func setupPickerAttachState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if runtime.GOOS != "windows" {
		base, err := os.MkdirTemp("/tmp", "axs")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(base) })
		t.Setenv("XDG_RUNTIME_DIR", base)
	}
	writeAppCfg(t, "mux = \"tmux\"\n")
}

func writeFreshDeadLiveRecord(t *testing.T, id, cmd string) {
	t.Helper()
	const deadPID = 99999999
	rec := fmt.Sprintf("%d\t%d\tdead-start-token\t%s", time.Now().Unix(), deadPID, cmd)
	if err := os.WriteFile(filepath.Join(axdir.State("live"), id), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Remove(id) })

	e, ok := live.Snapshot()[id]
	if !ok {
		t.Fatalf("live snapshot missing test fixture %q", id)
	}
	if e.Age > live.Fresh {
		t.Fatalf("test fixture is not fresh: %#v", e)
	}
	if live.Running(e) {
		t.Fatalf("fresh dead-pid fixture unexpectedly verifies as running: %#v", e)
	}
}

func TestPickerSingleLocalAliasReopensRealSessionID(t *testing.T) {
	setupPickerAttachState(t)
	const alias = "alias-session"
	const real = "11111111-2222-4333-8444-555555555555"
	if err := meta.SaveAlias(alias, real); err != nil {
		t.Fatal(err)
	}
	mx := &recordingWindowMux{}
	a := App{mux: mx}

	stderr := captureStderr(t, func() {
		a.act(nomuxChoice(session.Session{ID: alias, Harness: "claude"}))
	})

	if stderr != "" {
		t.Fatalf("picker attach wrote stderr: %s", stderr)
	}
	if len(mx.opens) != 1 {
		t.Fatalf("opens = %d, want 1: %#v", len(mx.opens), mx.opens)
	}
	open := mx.opens[0]
	if open.sessionID != real {
		t.Fatalf("opened sessionID = %q, want real id %q", open.sessionID, real)
	}
	if !open.focus {
		t.Fatal("single Enter reopen must focus the replacement window")
	}
	if strings.Contains(open.cmd, alias) {
		t.Fatalf("open command still references alias %q: %s", alias, open.cmd)
	}
	if !strings.Contains(open.cmd, real) || !shellCommandHasArg(open.cmd, "attach") {
		t.Fatalf("open command = %q, want an attach command for real id %q", open.cmd, real)
	}
}

func TestPickerSingleLocalExactSessionIDBeatsStaleAlias(t *testing.T) {
	setupPickerAttachState(t)
	const id = "alias-session"
	const other = "other-real-session"
	if err := meta.SaveAlias(id, other); err != nil {
		t.Fatal(err)
	}
	mx := &recordingWindowMux{loc: map[string]string{
		id:    "@right",
		other: "@wrong",
	}}
	a := App{mux: mx}
	selected := session.Session{ID: id, Harness: "claude"}
	choice := nomuxChoice(selected)
	choice.Sessions = []session.Session{selected, {ID: other, Harness: "claude"}}

	stderr := captureStderr(t, func() {
		a.act(choice)
	})

	if stderr != "" {
		t.Fatalf("picker attach wrote stderr: %s", stderr)
	}
	if len(mx.focuses) != 1 || mx.focuses[0] != "@right" {
		t.Fatalf("focuses = %#v, want exact selected row window @right", mx.focuses)
	}
	if len(mx.opens) != 0 {
		t.Fatalf("opens = %#v, want no replacement when exact row window exists", mx.opens)
	}
}

func TestPickerSingleLocalExactSessionIDNoWindowIgnoresStaleAliasWindow(t *testing.T) {
	setupPickerAttachState(t)
	const id = "alias-session"
	const other = "other-real-session"
	if err := meta.SaveAlias(id, other); err != nil {
		t.Fatal(err)
	}
	mx := &recordingWindowMux{loc: map[string]string{
		other: "@wrong",
	}}
	a := App{mux: mx}
	selected := session.Session{ID: id, Harness: "claude"}
	choice := nomuxChoice(selected)
	choice.Sessions = []session.Session{selected, {ID: other, Harness: "claude"}}

	stderr := captureStderr(t, func() {
		a.act(choice)
	})

	if stderr != "" {
		t.Fatalf("picker attach wrote stderr: %s", stderr)
	}
	if len(mx.focuses) != 0 {
		t.Fatalf("focuses = %#v, want stale alias target left alone", mx.focuses)
	}
	if len(mx.opens) != 1 {
		t.Fatalf("opens = %d, want exact row replacement: %#v", len(mx.opens), mx.opens)
	}
	open := mx.opens[0]
	if open.sessionID != id || !open.focus {
		t.Fatalf("open = %#v, want focused replacement for exact id %q", open, id)
	}
	if strings.Contains(open.cmd, other) || !strings.Contains(open.cmd, id) {
		t.Fatalf("open command = %q, want exact id %q without stale alias target %q", open.cmd, id, other)
	}
}

func TestPickerOpenWindowsExactSessionIDNoWindowIgnoresStaleAliasWindow(t *testing.T) {
	setupPickerAttachState(t)
	const id = "alias-session"
	const other = "other-real-session"
	if err := meta.SaveAlias(id, other); err != nil {
		t.Fatal(err)
	}
	mx := &recordingWindowMux{loc: map[string]string{
		other: "@wrong",
	}}
	a := App{mux: mx}
	selected := session.Session{ID: id, Harness: "claude"}
	sessions := []session.Session{selected, {ID: other, Harness: "claude"}}
	cfg := nomuxChoice().Config

	a.openWindows(cfg, harnessByName(cfg.Harnesses), nil, []session.Session{selected}, sessions)

	if len(mx.focuses) != 0 {
		t.Fatalf("focuses = %#v, want background reopen without focusing stale alias target", mx.focuses)
	}
	if len(mx.opens) != 1 {
		t.Fatalf("opens = %d, want exact row reopened: %#v", len(mx.opens), mx.opens)
	}
	open := mx.opens[0]
	if open.sessionID != id || open.focus {
		t.Fatalf("open = %#v, want background reopen for exact id %q", open, id)
	}
	if strings.Contains(open.cmd, other) || !strings.Contains(open.cmd, id) {
		t.Fatalf("open command = %q, want exact id %q without stale alias target %q", open.cmd, id, other)
	}
}

func TestPickerSingleLocalMissingWindowReopensLiveHolder(t *testing.T) {
	setupPickerAttachState(t)
	const id = "live-held"
	srv, err := hold.Serve(id, hold.ServeOpts{})
	if err != nil {
		t.Fatalf("create holder stand-in: %v", err)
	}
	defer srv.Close()
	live.Start(id, "claude --resume live-held")
	defer live.Remove(id)
	mx := &recordingWindowMux{}
	a := App{mux: mx}

	stderr := captureStderr(t, func() {
		a.act(nomuxChoice(session.Session{ID: id, Harness: "claude"}))
	})

	if stderr != "" {
		t.Fatalf("picker attach wrote stderr: %s", stderr)
	}
	if len(mx.opens) != 1 {
		t.Fatalf("opens = %d, want 1: %#v", len(mx.opens), mx.opens)
	}
	open := mx.opens[0]
	if open.sessionID != id || !open.focus {
		t.Fatalf("open = %#v, want focused replacement for %q", open, id)
	}
	if !shellCommandHasArg(open.cmd, "attach") || !strings.Contains(open.cmd, id) {
		t.Fatalf("open command = %q, want attach command for %q", open.cmd, id)
	}
}

func shellCommandHasArg(cmd, arg string) bool {
	return strings.Contains(cmd, " "+arg+" ") ||
		strings.Contains(cmd, "'"+arg+"'") ||
		strings.Contains(cmd, "\""+arg+"\"")
}

func TestPickerSingleLocalFreshButNotRunningHeartbeatOpensReplacement(t *testing.T) {
	setupPickerAttachState(t)
	const id = "fresh-dead"
	writeFreshDeadLiveRecord(t, id, "claude --resume fresh-dead")
	mx := &recordingWindowMux{}
	a := App{mux: mx}

	stderr := captureStderr(t, func() {
		a.act(nomuxChoice(session.Session{ID: id, Harness: "claude"}))
	})

	if stderr != "" {
		t.Fatalf("picker attach wrote stderr: %s", stderr)
	}
	if len(mx.opens) != 1 {
		t.Fatalf("opens = %d, want replacement window: %#v", len(mx.opens), mx.opens)
	}
	open := mx.opens[0]
	if open.sessionID != id || !open.focus {
		t.Fatalf("open = %#v, want focused replacement for %q", open, id)
	}
}

func TestPickerSingleLocalOpenFailureReported(t *testing.T) {
	setupPickerAttachState(t)
	const id = "open-fails"
	mx := &recordingWindowMux{openErr: errors.New("tmux is unavailable")}
	a := App{mux: mx}

	stderr := captureStderr(t, func() {
		a.act(nomuxChoice(session.Session{ID: id, Harness: "claude"}))
	})

	if len(mx.opens) != 1 {
		t.Fatalf("opens = %d, want one attempted replacement", len(mx.opens))
	}
	if !strings.Contains(stderr, "open tmux window") || !strings.Contains(stderr, "tmux is unavailable") {
		t.Fatalf("stderr = %q, want the tmux open failure", stderr)
	}
}

func TestAttachDirectFreshButNotRunningHeartbeatExecsReplacement(t *testing.T) {
	home := isolate(t)
	if runtime.GOOS != "windows" {
		base, err := os.MkdirTemp("/tmp", "axs")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(base) })
		t.Setenv("XDG_RUNTIME_DIR", base)
	}
	const id = "00000000-0000-0000-0000-00000000a742"
	writeClaudeTranscript(t, home, id, "still running")
	writeFreshDeadLiveRecord(t, id, "claude --resume "+id)

	var execs []string
	exitCode := -1
	origHeld, origExit := execHeldFn, exitFn
	execHeldFn = func(id, cmd string) { execs = append(execs, id+"\x00"+cmd) }
	exitFn = func(code int) { exitCode = code }
	t.Cleanup(func() {
		execHeldFn = origHeld
		exitFn = origExit
	})

	stderr := captureStderr(t, func() {
		App{mux: inactiveMux{}}.Attach([]string{id})
	})

	if stderr != "" {
		t.Fatalf("direct attach wrote stderr: %s", stderr)
	}
	if exitCode != -1 {
		t.Fatalf("exit code = %d, want no explicit exit", exitCode)
	}
	if len(execs) != 1 {
		t.Fatalf("direct attach execs = %#v, want one replacement holder", execs)
	}
	execID, execCmd, ok := strings.Cut(execs[0], "\x00")
	if !ok || execID != id || !strings.Contains(execCmd, id) {
		t.Fatalf("direct attach exec = %q, want resume for %q", execs[0], id)
	}
}

func TestAttachDirectAcceptsUniqueShortSessionID(t *testing.T) {
	home := isolate(t)
	const id = "12345678-0000-4000-8000-000000000001"
	writeClaudeTranscript(t, home, id, "short id target")

	var execs []string
	exitCode := -1
	origHeld, origExit := execHeldFn, exitFn
	execHeldFn = func(id, cmd string) { execs = append(execs, id+"\x00"+cmd) }
	exitFn = func(code int) { exitCode = code }
	t.Cleanup(func() {
		execHeldFn = origHeld
		exitFn = origExit
	})

	stderr := captureStderr(t, func() {
		App{mux: inactiveMux{}}.Attach([]string{"12345678"})
	})

	if stderr != "" {
		t.Fatalf("direct attach wrote stderr: %s", stderr)
	}
	if exitCode != -1 {
		t.Fatalf("exit code = %d, want no explicit exit", exitCode)
	}
	if len(execs) != 1 {
		t.Fatalf("direct attach execs = %#v, want one replacement holder", execs)
	}
	execID, execCmd, ok := strings.Cut(execs[0], "\x00")
	if !ok || execID != id || !strings.Contains(execCmd, id) {
		t.Fatalf("direct attach exec = %q, want resume for %q", execs[0], id)
	}
}

type attachCalled struct {
	id, cmd, adopt string
}

func TestAttachResolvesAliasBeforeHolderClient(t *testing.T) {
	setupPickerAttachState(t)
	const alias = "alias-session"
	const real = "11111111-2222-4333-8444-555555555555"
	if err := meta.SaveAlias(alias, real); err != nil {
		t.Fatal(err)
	}
	orig := attachHolderFn
	attachHolderFn = func(id, cmd, adopt string) { panic(attachCalled{id: id, cmd: cmd, adopt: adopt}) }
	defer func() { attachHolderFn = orig }()

	defer func() {
		c, ok := recover().(attachCalled)
		if !ok {
			t.Fatalf("attach did not enter holder client with captured call")
		}
		if c.id != real || c.cmd != "resume-real" || c.adopt != "" {
			t.Fatalf("holder call = %#v, want real id %q and original cmd", c, real)
		}
	}()
	App{}.Attach([]string{alias, "--cmd", "resume-real"})
}

func TestAttachCmdExactSessionIDBeatsStaleAlias(t *testing.T) {
	home := isolate(t)
	setupPickerAttachState(t)
	const id = "00000000-0000-0000-0000-000000000101"
	const other = "00000000-0000-0000-0000-000000000202"
	writeClaudeTranscript(t, home, id, "exact target")
	writeClaudeTranscript(t, home, other, "stale alias target")
	if err := meta.SaveAlias(id, other); err != nil {
		t.Fatal(err)
	}
	orig := attachHolderFn
	attachHolderFn = func(id, cmd, adopt string) { panic(attachCalled{id: id, cmd: cmd, adopt: adopt}) }
	defer func() { attachHolderFn = orig }()

	defer func() {
		c, ok := recover().(attachCalled)
		if !ok {
			t.Fatalf("attach did not enter holder client with captured call")
		}
		if c.id != id || c.cmd != "resume-exact" || c.adopt != "" {
			t.Fatalf("holder call = %#v, want exact id %q and original cmd", c, id)
		}
	}()
	App{}.Attach([]string{id, "--cmd", "resume-exact"})
}

func TestAttachReadsAndRemovesCommandFile(t *testing.T) {
	home := isolate(t)
	setupPickerAttachState(t)
	const id = "00000000-0000-0000-0000-000000000303"
	writeClaudeTranscript(t, home, id, "exact target")
	path := filepath.Join(t.TempDir(), "cmd.txt")
	if err := os.WriteFile(path, []byte("resume-from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := attachHolderFn
	attachHolderFn = func(id, cmd, adopt string) { panic(attachCalled{id: id, cmd: cmd, adopt: adopt}) }
	defer func() { attachHolderFn = orig }()

	defer func() {
		c, ok := recover().(attachCalled)
		if !ok {
			t.Fatalf("attach did not enter holder client with captured call")
		}
		if c.id != id || c.cmd != "resume-from-file" || c.adopt != "" {
			t.Fatalf("holder call = %#v, want exact id %q and command file contents", c, id)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("command file still exists after attach parse: %v", err)
		}
	}()
	App{}.Attach([]string{id, "--cmd-file", path})
}
