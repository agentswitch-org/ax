//go:build unix

package hold

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// The menu-chord helper's observable signal: openMenu prints this marker and
// exits with this code instead of exec-replacing into the picker (see TestMain).
const (
	menuReopenMarker = "AX_MENU_REOPEN"
	menuReopenCode   = 42
)

// TestMain doubles as the attach-client helper. Re-exec'd with
// AX_HOLD_PTY_HELPER=<id> and its stdio on a real pty, the test binary runs
// the attach client exactly as `ax attach` does and exits with its code, so
// the tests can drive individual keystrokes into the pty master and observe
// what a real terminal user gets: raw-mode char-by-char input, detach, exit.
func TestMain(m *testing.M) {
	if id := os.Getenv("AX_HOLD_PTY_HELPER"); id != "" {
		// openMenu stands in for the app layer's exec-replace into bare `ax`: in
		// production it hands the terminal to the picker and never returns, so here
		// it prints a marker the test can see on the client terminal and exits with
		// a distinct code. Reaching it proves the menu chord tore the client down
		// (terminal restored, holder still alive) then routed to the picker.
		openMenu := func() {
			os.Stdout.WriteString(menuReopenMarker + "\r\n")
			os.Exit(menuReopenCode)
		}
		code, err := Attach(id, nil, openMenu)
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper:", err)
			os.Exit(3)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// echoHarness is the minimal raw-mode "harness" under the holder: it records
// every INPUT byte the holder forwards and echoes it straight back through the
// holder's output tee, like a full-screen TUI that responds to each keystroke.
type echoHarness struct {
	mu  sync.Mutex
	got []byte
	srv *Server
}

func (e *echoHarness) Write(p []byte) (int, error) {
	e.mu.Lock()
	e.got = append(e.got, p...)
	srv := e.srv
	e.mu.Unlock()
	if srv != nil {
		srv.Output(p)
	}
	return len(p), nil
}

func (e *echoHarness) String() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return string(e.got)
}

// ptyClient is one live attach client on a real pty: the holder it talks to,
// the echo harness behind it, the pty master driving its terminal, and the
// captured client output.
type ptyClient struct {
	srv    *Server
	echo   *echoHarness
	cmd    *exec.Cmd
	master *os.File
	waited chan error

	mu  sync.Mutex
	out []byte
}

func (c *ptyClient) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.out)
}

// startPTYClient stands the whole chain up: holder (echo harness) on a private
// socket dir, then the re-exec'd attach client with a fresh pty as its
// controlling terminal, connected and forwarding.
func startPTYClient(t *testing.T, id string) *ptyClient {
	return startPTYClientCfg(t, id, "")
}

// startPTYClientCfg is startPTYClient with an ax config for the client: cfg is
// written to a private config.toml the client reads via AX_CONFIG. "" means no
// config file, which still points AX_CONFIG at the (absent) private path so a
// developer's real ~/.config/ax/config.toml never leaks into the tests.
func startPTYClientCfg(t *testing.T, id, cfg string) *ptyClient {
	t.Helper()
	// A short private socket dir: t.TempDir under a macOS TMPDIR plus the test
	// name can push a unix socket path past its ~104-byte limit.
	dir, err := os.MkdirTemp("/tmp", "axhold")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
	cfgPath := dir + "/config.toml"
	if cfg != "" {
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}

	echo := &echoHarness{}
	srv, err := Serve(id, ServeOpts{Input: echo, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	echo.mu.Lock()
	echo.srv = srv
	echo.mu.Unlock()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "AX_HOLD_PTY_HELPER="+id, "XDG_RUNTIME_DIR="+dir, "AX_CONFIG="+cfgPath)
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	c := &ptyClient{srv: srv, echo: echo, cmd: cmd, master: master, waited: make(chan error, 1)}
	go func() { c.waited <- cmd.Wait() }()
	// Drain the master continuously (capturing the client's screen) so the
	// client never blocks on a full pty output buffer.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				c.mu.Lock()
				c.out = append(c.out, buf[:n]...)
				c.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		master.Close()
	})
	// The client is driving the terminal once it is in the holder's client set
	// AND its input loop is up. A byte flowing through proves both; waiting for
	// the attach handshake alone would race the client's raw-mode and signal
	// setup, which happen after it.
	waitFor(t, "client attached", func() bool { return len(srv.clientList()) == 1 })
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// exitCode waits for the helper to finish and returns its exit code.
func (c *ptyClient) exitCode(t *testing.T) int {
	t.Helper()
	select {
	case err := <-c.waited:
		if err == nil {
			return 0
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		t.Fatalf("client wait: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("client did not exit")
	}
	return -1
}

// The frozen-keys regression: with the client terminal in true raw mode and
// nothing else reading it, every keystroke written to the client pty must
// reach the harness immediately, one byte at a time, with no newline ever
// sent. Canonical (line-buffered) input, a failed MakeRaw, or a competing
// reader stealing bytes all fail this by timing out with keys undelivered.
func TestAttachForwardsKeystrokesCharByChar(t *testing.T) {
	c := startPTYClient(t, "pty-echo")

	want := ""
	for _, b := range []byte{'h', 'x', 'q'} {
		if _, err := c.master.Write([]byte{b}); err != nil {
			t.Fatalf("write %q: %v", b, err)
		}
		want += string(b)
		expect := want
		waitFor(t, fmt.Sprintf("key %q to reach the harness", b), func() bool { return c.echo.String() == expect })
	}

	// Ctrl-D must arrive as a plain 0x04 byte, not act as a canonical EOF that
	// closes the client's stdin: the client stays alive and keeps forwarding.
	c.master.Write([]byte{0x04})
	waitFor(t, "ctrl-d to reach the harness as a byte", func() bool { return c.echo.String() == "hxq\x04" })
	if err := c.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("client died on ctrl-d: %v", err)
	}
	// Ctrl-S must arrive as a plain 0x13 byte, not act as XOFF freezing the
	// tty: software flow control is off on the client terminal. Ctrl-Q (XON)
	// likewise arrives as a plain 0x11 byte, and Ctrl-G (the pre-chord detach
	// default) is an ordinary harness byte now: all forwarded, none scanned out.
	c.master.Write([]byte{0x13, 'z'})
	waitFor(t, "ctrl-s to reach the harness as a byte", func() bool { return c.echo.String() == "hxq\x04\x13z" })
	c.master.Write([]byte{0x11, 'w'})
	waitFor(t, "ctrl-q to reach the harness as a byte", func() bool { return c.echo.String() == "hxq\x04\x13z\x11w" })
	c.master.Write([]byte{0x07})
	waitFor(t, "ctrl-g to reach the harness as a byte", func() bool { return c.echo.String() == "hxq\x04\x13z\x11w\x07" })

	// The echo round trip: harness output reaches the client's terminal.
	waitFor(t, "echo on the client terminal", func() bool { return strings.Contains(c.output(), "hxq") })

	c.master.Write([]byte{DefaultDetachPrefix, DefaultDetachLetter})
	if code := c.exitCode(t); code != 0 {
		t.Fatalf("detach exit code = %d, want 0", code)
	}
}

// On detach the client must hand the terminal back with the reporting modes a
// harness may have left on (mouse/focus/paste) turned off, or the shell prompt
// fills with leaked reports. The exact disable bytes must reach the client
// terminal as part of the teardown.
func TestDetachDisablesReportingModes(t *testing.T) {
	c := startPTYClient(t, "pty-reset")

	c.master.Write([]byte("hi\x01d"))
	if code := c.exitCode(t); code != 0 {
		t.Fatalf("detach exit code = %d, want 0", code)
	}
	waitFor(t, "holder to drop the client", func() bool { return len(c.srv.clientList()) == 0 })
	for _, want := range []string{
		"\x1b[?1000l", "\x1b[?1002l", "\x1b[?1003l", "\x1b[?1006l", // mouse
		"\x1b[?1004l", // focus
		"\x1b[?2004l", // bracketed paste
		"\x1b[?9001l", // Windows win32-input-mode
	} {
		if !strings.Contains(c.output(), want) {
			t.Fatalf("detach output missing reporting-mode disable %q; output: %q", want, c.output())
		}
	}
}

// The scroll-in-held-session fix, end to end on a real pty: when the held
// harness enables mouse tracking, focus reporting and bracketed paste, those
// enables must reach the real client terminal (so the scroll wheel and paste
// work inside the harness), while win32-input-mode and the kitty keyboard
// enable must still be scrubbed (they re-encode input and would break the detach
// chord). The chord must still detach, and teardown must disable the mouse modes
// it let through so nothing leaks to the shell.
func TestAttachPassesMouseEnableAndScrubsKeyboard(t *testing.T) {
	c := startPTYClient(t, "pty-mouse")

	// The harness turns on mouse tracking (click + SGR), focus and paste (all
	// should pass), plus a win32-input-mode enable and a kitty keyboard push
	// (both must be scrubbed).
	c.srv.Output([]byte("\x1b[?1000h\x1b[?1006h\x1b[?1004h\x1b[?2004h\x1b[?9001h\x1b[>1u"))

	waitFor(t, "mouse/focus/paste enables to reach the client terminal", func() bool {
		o := c.output()
		return strings.Contains(o, "\x1b[?1000h") && strings.Contains(o, "\x1b[?1006h") &&
			strings.Contains(o, "\x1b[?1004h") && strings.Contains(o, "\x1b[?2004h")
	})
	if strings.Contains(c.output(), "\x1b[?9001h") {
		t.Fatalf("win32-input enable leaked to the terminal; output: %q", c.output())
	}
	if strings.Contains(c.output(), "\x1b[>1u") {
		t.Fatalf("kitty keyboard enable leaked to the terminal; output: %q", c.output())
	}

	// The detach chord still works (keyboard protocols still scrubbed), and the
	// teardown reset disables the mouse modes we let through.
	c.master.Write([]byte("\x01d"))
	if code := c.exitCode(t); code != 0 {
		t.Fatalf("detach exit code = %d, want 0", code)
	}
	if !strings.Contains(c.output(), "\x1b[?1000l") || !strings.Contains(c.output(), "\x1b[?1006l") {
		t.Fatalf("teardown did not disable mouse tracking; output: %q", c.output())
	}
}

// A terminal resize delivers SIGWINCH while the attach client is blocked in
// its stdin read loop. That interrupt must resize the holder and keep the
// viewer attached, not detach and close the pane.
func TestAttachSurvivesResizeSIGWINCH(t *testing.T) {
	c := startPTYClient(t, "pty-resize")

	c.master.Write([]byte{'a'})
	waitFor(t, "initial byte to reach the harness", func() bool { return c.echo.String() == "a" })

	if err := pty.Setsize(c.master, &pty.Winsize{Rows: 12, Cols: 60}); err != nil {
		t.Fatalf("resize pty: %v", err)
	}
	if err := c.cmd.Process.Signal(syscall.SIGWINCH); err != nil {
		t.Fatalf("sigwinch: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if clients := len(c.srv.clientList()); clients != 1 {
		t.Fatalf("client detached after resize/SIGWINCH; clients=%d", clients)
	}

	c.master.Write([]byte{'b'})
	waitFor(t, "post-resize byte to reach the harness", func() bool { return c.echo.String() == "ab" })
	if err := c.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("client died after resize/SIGWINCH: %v", err)
	}

	c.master.Write([]byte{DefaultDetachPrefix, DefaultDetachLetter})
	if code := c.exitCode(t); code != 0 {
		t.Fatalf("detach exit code = %d, want 0", code)
	}
}

// The Ctrl-A d chord detaches: bytes before it in the same read still reach
// the harness, neither chord byte ever does, the client exits 0, and the
// holder keeps running for the next attach.
func TestDetachChordKeepsHolderAlive(t *testing.T) {
	c := startPTYClient(t, "pty-detach")

	c.master.Write([]byte("hi\x01d"))
	if code := c.exitCode(t); code != 0 {
		t.Fatalf("detach exit code = %d, want 0", code)
	}
	if got := c.echo.String(); got != "hi" {
		t.Fatalf("harness got %q, want %q (leading bytes forwarded, chord scanned out)", got, "hi")
	}
	waitFor(t, "holder to drop the client", func() bool { return len(c.srv.clientList()) == 0 })
	if !Probe("pty-detach") {
		t.Fatalf("holder gone after detach; it must keep running")
	}
	if !strings.Contains(c.output(), "detached") {
		t.Fatalf("client did not report the detach; output: %q", c.output())
	}
	if !strings.Contains(c.output(), "Ctrl-A then d to detach") {
		t.Fatalf("client did not print the attach hint; output: %q", c.output())
	}
}

// The Ctrl-A a menu chord detaches exactly like Ctrl-A d (leading bytes reach
// the harness, neither chord byte does, the holder keeps running) but then
// routes into the picker reopen instead of returning to the shell: the client
// hits openMenu (marker on the terminal, distinct exit code) rather than
// printing the plain detach line.
func TestMenuChordReopensPickerAndKeepsHolderAlive(t *testing.T) {
	c := startPTYClient(t, "pty-menu")

	c.master.Write([]byte("hi\x01a"))
	if code := c.exitCode(t); code != menuReopenCode {
		t.Fatalf("menu chord exit code = %d, want %d (openMenu reached)", code, menuReopenCode)
	}
	if got := c.echo.String(); got != "hi" {
		t.Fatalf("harness got %q, want %q (leading bytes forwarded, chord scanned out)", got, "hi")
	}
	waitFor(t, "holder to drop the client", func() bool { return len(c.srv.clientList()) == 0 })
	if !Probe("pty-menu") {
		t.Fatalf("holder gone after menu chord; it must keep running")
	}
	if !strings.Contains(c.output(), menuReopenMarker) {
		t.Fatalf("client did not reach the picker-reopen path; output: %q", c.output())
	}
	if strings.Contains(c.output(), "detached; the session keeps running") {
		t.Fatalf("menu chord printed the plain detach line; it should reopen the picker; output: %q", c.output())
	}
}

// A menu chord split across reads still routes to the picker: the prefix
// arrives alone, then the menu letter in a later read. The armed state must
// persist across reads for the menu letter just as it does for detach.
func TestMenuChordSplitAcrossReads(t *testing.T) {
	c := startPTYClient(t, "pty-menu-split")

	c.master.Write([]byte("ok"))
	waitFor(t, "input flowing", func() bool { return c.echo.String() == "ok" })

	c.master.Write([]byte{0x01})
	time.Sleep(50 * time.Millisecond) // let the prefix land as its own read
	c.master.Write([]byte{'a'})
	if code := c.exitCode(t); code != menuReopenCode {
		t.Fatalf("split menu-chord exit code = %d, want %d", code, menuReopenCode)
	}
	if got := c.echo.String(); got != "ok" {
		t.Fatalf("harness got %q, want %q (neither chord byte forwarded)", got, "ok")
	}
	if !Probe("pty-menu-split") {
		t.Fatalf("holder gone after split menu chord; it must keep running")
	}
}

// A configured menu_key is honored: with menu_key = "m", Ctrl-A then m reopens
// the picker and the default Ctrl-A then a is an ordinary prefix-then-letter
// passthrough (both bytes forwarded, no reopen).
func TestConfiguredMenuKey(t *testing.T) {
	c := startPTYClientCfg(t, "pty-menu-config", "menu_key = \"m\"\n")

	// Ctrl-A then a is no longer the menu chord: prefix-other forwards both.
	c.master.Write([]byte{0x01, 'a'})
	waitFor(t, "ctrl-a a to forward both bytes", func() bool { return c.echo.String() == "\x01a" })

	c.master.Write([]byte{0x01, 'm'})
	if code := c.exitCode(t); code != menuReopenCode {
		t.Fatalf("configured menu chord exit code = %d, want %d", code, menuReopenCode)
	}
	if got := c.echo.String(); got != "\x01a" {
		t.Fatalf("harness got %q, want %q (configured menu chord scanned out)", got, "\x01a")
	}
	if !Probe("pty-menu-config") {
		t.Fatalf("holder gone after configured menu chord; it must keep running")
	}
}

// A chord split across reads still detaches: the prefix arrives alone (its
// own read at the client), then the letter in a later read. The armed state
// must persist across reads.
func TestDetachChordSplitAcrossReads(t *testing.T) {
	c := startPTYClient(t, "pty-split")

	c.master.Write([]byte("ok"))
	waitFor(t, "input flowing", func() bool { return c.echo.String() == "ok" })

	c.master.Write([]byte{0x01})
	time.Sleep(50 * time.Millisecond) // let the prefix land as its own read
	c.master.Write([]byte{'d'})
	if code := c.exitCode(t); code != 0 {
		t.Fatalf("split-chord detach exit code = %d, want 0", code)
	}
	if got := c.echo.String(); got != "ok" {
		t.Fatalf("harness got %q, want %q (neither chord byte forwarded)", got, "ok")
	}
	if !Probe("pty-split") {
		t.Fatalf("holder gone after split-chord detach; it must keep running")
	}
}

// Pressing the prefix twice sends exactly one literal prefix byte to the
// harness and does not detach; prefix-then-other forwards both bytes, so no
// harness key is ever stolen, only delayed by one keystroke.
func TestDetachPrefixLiteralAndPassthrough(t *testing.T) {
	c := startPTYClient(t, "pty-literal")

	c.master.Write([]byte{0x01, 0x01})
	waitFor(t, "double prefix to deliver one literal ctrl-a", func() bool { return c.echo.String() == "\x01" })
	c.master.Write([]byte{'z'})
	waitFor(t, "the literal to disarm the chord", func() bool { return c.echo.String() == "\x01z" })

	c.master.Write([]byte{0x01, 'x'})
	waitFor(t, "prefix-other to forward both bytes", func() bool { return c.echo.String() == "\x01z\x01x" })
	if err := c.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("client died on the literal prefix: %v", err)
	}

	c.master.Write([]byte{0x01, 'd'})
	if code := c.exitCode(t); code != 0 {
		t.Fatalf("detach exit code = %d, want 0", code)
	}
	if got := c.echo.String(); got != "\x01z\x01x" {
		t.Fatalf("harness got %q, want %q (chord bytes never forwarded)", got, "\x01z\x01x")
	}
}

// Ctrl-backslash (the historic dtach detach key) stays an always-on fallback:
// the raw 0x1c byte detaches exactly like the configured key.
func TestDetachFallbackByteKeepsHolderAlive(t *testing.T) {
	c := startPTYClient(t, "pty-fallback")

	// Send "yo" first and wait for it to arrive: this proves raw mode is active
	// and the chord-scanner goroutine is running before we send 0x1c (Ctrl-\).
	// Without this gate, 0x1c can arrive at the pty while ISIG is still on (the
	// client has not called MakeRaw yet), causing the kernel to deliver SIGQUIT
	// instead of the raw byte, which triggers Go's default handler (exit 2).
	c.master.Write([]byte("yo"))
	waitFor(t, "probe bytes to reach harness", func() bool { return c.echo.String() == "yo" })
	c.master.Write([]byte{0x1c})
	if code := c.exitCode(t); code != 0 {
		t.Fatalf("fallback detach exit code = %d, want 0", code)
	}
	if got := c.echo.String(); got != "yo" {
		t.Fatalf("harness got %q, want %q (prefix forwarded, fallback key scanned out)", got, "yo")
	}
	waitFor(t, "holder to drop the client", func() bool { return len(c.srv.clientList()) == 0 })
	if !Probe("pty-fallback") {
		t.Fatalf("holder gone after fallback detach; it must keep running")
	}
}

// A configured detach_prefix is honored: with detach_prefix = "ctrl-b" the
// default Ctrl-A is an ordinary byte forwarded to the harness (only the
// configured prefix arms the chord) and Ctrl-B then d detaches through the
// shared path.
func TestConfiguredDetachPrefix(t *testing.T) {
	c := startPTYClientCfg(t, "pty-config", "detach_prefix = \"ctrl-b\"\n")

	c.master.Write([]byte("\x01a"))
	waitFor(t, "ctrl-a to reach the harness as a byte", func() bool { return c.echo.String() == "\x01a" })

	c.master.Write([]byte{0x02, 'd'})
	if code := c.exitCode(t); code != 0 {
		t.Fatalf("configured detach exit code = %d, want 0", code)
	}
	if got := c.echo.String(); got != "\x01a" {
		t.Fatalf("harness got %q, want %q (configured chord scanned out)", got, "\x01a")
	}
	waitFor(t, "holder to drop the client", func() bool { return len(c.srv.clientList()) == 0 })
	if !Probe("pty-config") {
		t.Fatalf("holder gone after configured detach; it must keep running")
	}
	if !strings.Contains(c.output(), "Ctrl-B then d to detach") {
		t.Fatalf("attach hint does not show the configured chord; output: %q", c.output())
	}
}

// Ctrl-backslash arriving as SIGQUIT (a terminal whose raw mode did not take
// turns the keystroke into the signal instead of the byte) detaches the same
// way: client exits 0, holder survives. Without a SIGQUIT handler the default
// action kills the client with a non-zero status.
func TestDetachSIGQUITKeepsHolderAlive(t *testing.T) {
	c := startPTYClient(t, "pty-sigquit")

	// A byte through the loop proves the client finished its signal setup.
	c.master.Write([]byte{'a'})
	waitFor(t, "input flowing", func() bool { return c.echo.String() == "a" })

	if err := c.cmd.Process.Signal(syscall.SIGQUIT); err != nil {
		t.Fatalf("sigquit: %v", err)
	}
	if code := c.exitCode(t); code != 0 {
		t.Fatalf("sigquit detach exit code = %d, want 0", code)
	}
	waitFor(t, "holder to drop the client", func() bool { return len(c.srv.clientList()) == 0 })
	if !Probe("pty-sigquit") {
		t.Fatalf("holder gone after sigquit detach; it must keep running")
	}
}
