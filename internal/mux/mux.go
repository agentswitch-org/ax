// Package mux drives the terminal multiplexer where running sessions live as
// windows. Multiplexer is the contract; tmux is the default implementation and
// zellij is an alternative. process runs sessions as plain OS subprocesses for a
// host with no multiplexer at all. New picks one from the `mux` config setting.
//
// tmux and zellij additionally namespace every window, session, or tab they
// create with an ax prefix (see prefixName), so ax's own windows stand out
// from the user's own in the native tmux status bar or zellij tab bar and can
// be bulk-selected or killed by a simple prefix match. This is purely a
// mux-level naming convention: it never touches the picker's own display name
// for a session (session.Session.Name / view's title column), which stays
// whatever the user set. process and none have no window/session/tab to name,
// so prefixing does not apply to them.
package mux

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/shell"
)

// submitDelay is the pause between delivering typed text and the Enter that
// submits it. Some harness TUIs (codex) coalesce a text burst and an
// immediately-following Enter into a single paste event and do NOT submit, so a
// scripted `ax send` would type the prompt but leave it sitting in the composer.
// A short gap lets the TUI settle so the Enter registers as a deliberate submit,
// exactly as a human typing then pressing return. Harmless for claude/pi, which
// submit either way.
const submitDelay = 150 * time.Millisecond

// Multiplexer opens, locates, and focuses session windows.
type Multiplexer interface {
	// Active reports whether we are running inside the multiplexer.
	Active() bool
	// HasWindows reports whether this backend has focusable/closable viewer
	// windows. The process backend is active but intentionally returns false.
	HasWindows() bool
	// Open runs cmd in a new window for the session in dir, tagging it so Locate
	// can find it again. focus switches to it; otherwise it opens in the background.
	// target, when non-empty, is the unprefixed mux session the window should be
	// born in (created if missing), so related sessions cluster together; "" opens
	// in the current session (today's flat behavior). Only a backend that can place
	// a window into a named session honors target; every other backend ignores it
	// and falls back to flat placement, exactly as MoveWindow already degrades.
	Open(dir, title, cmd, sessionID, target string, focus bool) error
	// Locate returns the window running sessionID, if one is open.
	Locate(sessionID string) (window string, ok bool)
	// Live maps each open session id to a short "session:window.pane" locator,
	// for the picker's location column.
	Live() map[string]string
	// Panes lists every open pane with the data needed to correlate a window ax
	// did not launch with the session running in it (see internal/adopt).
	Panes() []Pane
	// Focus switches to a window.
	Focus(window string) error
	// Send types text into the window running sessionID; enter submits it.
	Send(sessionID, text string, enter bool) error
	// Interrupt sends a ctrl-c into the window running sessionID, to redirect a
	// worker mid-turn without killing it.
	Interrupt(sessionID string) error
	// PaneTail returns the last lines of the pane running sessionID, for detecting
	// a worker blocked on a sub-prompt (permission y/n, an OAuth login).
	PaneTail(sessionID string, lines int) string
	// MoveWindow moves the window running sessionID into the named multiplexer
	// session (created if missing), so related agents can be sorted together.
	MoveWindow(sessionID, target string) error
	// CloseWindow closes the window running sessionID. For a dtach-held session
	// (the default launch path) this only detaches the holder: the harness
	// process survives on its dtach socket, so reopening the session reattaches
	// it. For a session that is NOT held, closing the window kills the process, so
	// the caller must guard against that (see app.windowDetachSafe); this method
	// itself just closes the window it is told to.
	CloseWindow(sessionID string) error
	// Retag points the current window (the one this process runs in) at sessionID,
	// replacing the placeholder tag set when it opened. Called once a
	// mint-its-own-id harness (codex, opencode) reveals its real session id.
	Retag(sessionID string) error
}

// Pane is one open multiplexer pane, for correlating hand-started windows.
type Pane struct {
	Window  string // window id, for Focus
	Locator string // "session:window.pane", for the WIN column
	Tag     string // @ax_session, set when ax launched it
	Start   string // pane start command (may embed the session id)
	Cmd     string // current foreground command (harness detection)
	Cwd     string // pane working directory
	PID     int    // pane process id, for its start time
}

// New returns the multiplexer named by the `mux` config setting: tmux (the Unix
// default), zellij, process (the Windows default), or none.
func New() Multiplexer {
	cfg, _ := config.Load()
	return backend(cfg.Mux)
}

// backend maps a config value to a Multiplexer. The platform seams pick the
// concrete backend: "process" is the FIFO/setsid implementation on unix and
// the holder-pipe implementation on Windows (processMux), and an unknown or
// empty value selects the platform default (defaultMux: tmux on unix, so an
// unset config keeps today's behavior; the process backend on Windows, where
// no multiplexer exists to shell out to). Split out so the selector is unit-
// testable without touching config on disk.
func backend(name string) Multiplexer {
	switch name {
	case "zellij":
		return zellij{}
	case "process":
		return processMux()
	case "none":
		return none{}
	default:
		return defaultMux(name)
	}
}

// EffectiveName reports the backend name a config value resolves to on this
// platform. It mirrors backend/defaultMux without constructing a backend, so
// status reports can describe the real runtime default instead of the raw,
// often-empty config value.
func EffectiveName(name string) string {
	return effectiveName(name, runtime.GOOS)
}

func effectiveName(name, goos string) string {
	switch name {
	case "zellij", "process", "none", "tmux":
		return name
	}
	if goos == "windows" {
		return "process"
	}
	return "tmux"
}

// IsProcess reports whether the configured mux resolves to the process
// (no-multiplexer) backend on this platform, so the launcher can pick the bare
// `ax run` command a process backend spawns directly (skipping the attach-client
// wrap that needs a terminal).
func IsProcess() bool {
	cfg, _ := config.Load()
	return isProcessMux(cfg.Mux)
}

// resolvePrefix maps the `mux_prefix` config value to the literal ax namespace
// prefix a tmux/zellij backend stamps on a window, session, or tab name it
// creates. "" (unset) is the default "ax:"; "off" turns prefixing off
// entirely; anything else is used as the literal prefix. Split out so the
// resolution is unit-testable without touching config on disk.
func resolvePrefix(raw string) string {
	switch raw {
	case "":
		return "ax:"
	case "off":
		return ""
	default:
		return raw
	}
}

// namePrefix reads the configured ax namespace prefix.
func namePrefix() string {
	cfg, _ := config.Load()
	return resolvePrefix(cfg.MuxPrefix)
}

// prefixName stamps the ax namespace prefix on a window/session/tab name a
// backend is about to create, so it stands out from the user's own in the
// native tmux/zellij UI and every ax-managed one is a simple prefix match away
// (for bulk-selecting or killing). An empty name (nothing to title) is left
// alone, and prefixing already applied (an already-open ax window being
// renamed, or a name a caller already prefixed) is not stacked twice.
func prefixName(name string) string {
	if name == "" {
		return name
	}
	if p := namePrefix(); p != "" && !strings.HasPrefix(name, p) {
		return p + name
	}
	return name
}

// none is the no-multiplexer backend: it reports inactive and every operation is
// a no-op. It is the honest answer when ax runs outside any multiplexer and the
// user has not opted into a real process backend (that is a separate build).
type none struct{}

func (none) Active() bool                            { return false }
func (none) HasWindows() bool                        { return false }
func (none) Open(_, _, _, _, _ string, _ bool) error { return nil }
func (none) Locate(string) (string, bool)            { return "", false }
func (none) Live() map[string]string                 { return nil }
func (none) Panes() []Pane                           { return nil }
func (none) Focus(string) error                      { return nil }
func (none) Send(string, string, bool) error         { return nil }
func (none) Interrupt(string) error                  { return nil }
func (none) PaneTail(string, int) string             { return "" }
func (none) MoveWindow(string, string) error         { return nil }
func (none) CloseWindow(string) error                { return nil }
func (none) Retag(string) error                      { return nil }

type tmux struct{}

func (tmux) Active() bool     { return os.Getenv("TMUX") != "" }
func (tmux) HasWindows() bool { return true }

func (t tmux) Open(dir, title, cmd, sessionID, target string, focus bool) error {
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("refusing to open empty tmux command for session %s", sessionID)
	}
	// A non-empty target names the mux session the window is born in. Ensure it
	// exists first, then new-window -t it there; on a focused launch, follow the
	// human in with switch-client so they land where the window opened. A -d
	// background launch places only. An empty target keeps today's flat path:
	// new-window in the current session.
	target = prefixName(target)
	var stray string
	if target != "" {
		var err error
		if stray, err = ensureSession(target); err != nil {
			return err
		}
	}
	// tmux caps a new-window command message at ~16 KB; a launch carrying a large
	// --behavior (tens of KB of system prompt embedded in the command) blows past
	// it and tmux rejects the whole window with "command too long". Spill an
	// oversized command to a self-deleting temp script and open the window on the
	// short `sh <file>` instead, so an arbitrarily large behavior launches intact.
	cmd, cleanup, err := spillLongCmd(cmd)
	if err != nil {
		return err
	}
	out, err := exec.Command("tmux", windowArgs(dir, prefixName(title), cmd, target, focus)...).CombinedOutput()
	if err != nil {
		cleanup() // new-window never ran the script, so it will not self-delete
		return tmuxCmdError("tmux new-window", err, out)
	}
	wid := strings.TrimSpace(string(out))
	if wid != "" && sessionID != "" {
		exec.Command("tmux", "set-option", "-w", "-t", wid, "@ax_session", sessionID).Run()
	}
	// The real ax window is now in the session, so the initial default-shell
	// window ensureSession created is safe to remove (the session outlives it).
	killStrayWindow(stray)
	if target != "" && focus {
		focusTarget := wid
		if focusTarget == "" {
			focusTarget = tmuxSessionName(target)
		}
		if out, err := exec.Command("tmux", switchClientArgs(focusTarget)...).CombinedOutput(); err != nil {
			return tmuxCmdError("tmux switch-client", err, out)
		}
	}
	return nil
}

func tmuxCmdError(op string, err error, out []byte) error {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return fmt.Errorf("%s: %w", op, err)
	}
	return fmt.Errorf("%s: %w: %s", op, err, msg)
}

// windowArgs builds `tmux new-window -P -F #{window_id} [-d] -n <title> [-c
// dir] [-t target:] <cmd>`. title already carries any ax namespace prefix, as
// does target; this only assembles the tmux argv. An empty target opens in the
// current session (flat).
func windowArgs(dir, title, cmd, target string, focus bool) []string {
	args := []string{"new-window", "-P", "-F", "#{window_id}"}
	if !focus {
		args = append(args, "-d")
	}
	args = append(args, "-n", title)
	if dir != "" {
		args = append(args, "-c", dir)
	}
	if target != "" {
		args = append(args, "-t", tmuxSessionName(target)+":")
	}
	return append(args, cmd)
}

// spillCmdThreshold is the command length above which Open spills the command to
// a temp script rather than passing it to tmux inline. tmux rejects a new-window
// command message over ~16 KB ("command too long"); the threshold sits well under
// that so the fixed new-window flags, the window title, and the -c directory
// (all also part of the message) still fit alongside a command right at the cap.
const spillCmdThreshold = 12 * 1024

// spillLongCmd keeps a normal (short) command inline but writes an oversized one
// to a self-deleting temp script and returns `sh <file>` in its place, so a large
// --behavior (or any long command) does not exceed tmux's new-window length cap.
// The returned cleanup removes the temp file; it is a no-op for the inline path
// and for a spilled command that ran (the script rm's itself first thing), and is
// only needed to reclaim the file when new-window never launches the script.
func spillLongCmd(cmd string) (out string, cleanup func(), err error) {
	if len(cmd) <= spillCmdThreshold {
		return cmd, func() {}, nil
	}
	f, err := os.CreateTemp("", "ax-win-*.sh")
	if err != nil {
		return "", func() {}, fmt.Errorf("tmux new-window: spill command: %w", err)
	}
	// `rm -f "$0"` first: on Unix the shell keeps reading from its open fd after
	// the path is unlinked, so the running script executes to completion and
	// leaves nothing behind, even though it exec's into the harness at the end.
	if _, err := fmt.Fprintf(f, "rm -f \"$0\"\n%s\n", cmd); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, fmt.Errorf("tmux new-window: spill command: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", func() {}, fmt.Errorf("tmux new-window: spill command: %w", err)
	}
	name := f.Name()
	return "sh " + shell.QuotePosix(name), func() { os.Remove(name) }, nil
}

// tmuxSessionName returns the name tmux will actually store when a session is
// created with new-session -s s. tmux 3.6b replaces colons and dots with
// underscores at creation time, so every subsequent reference (has-session,
// new-window -t, switch-client, move-window -t) must use this sanitized form
// or tmux misparses the target as "session:window.pane" and errors.
//
// Verified empirically on tmux 3.6b: "ax:foo" → "ax_foo", "ax.foo" → "ax_foo".
// Spaces are not sanitized by tmux and are passed through unchanged.
func tmuxSessionName(s string) string {
	return strings.NewReplacer(":", "_", ".", "_").Replace(s)
}

// ensureSession creates a detached tmux session named target when it does not
// already exist ("=" pins has-session to an exact name), so a window can be
// opened or moved into it. target is expected to be namespace-prefixed already.
// tmuxSessionName converts it to the form tmux actually stores before comparing
// and creating, so "ax:proj" and "ax_proj" are treated as the same session.
//
// new-session -d always spawns an initial default-shell window (index 0); that
// window is a stray the moment the real ax window is opened or moved in beside
// it, so ensureSession returns its window id (captured via -P -F) as stray. The
// caller kills it AFTER the real window lands, never before (killing a session's
// last window destroys the session). stray is "" when the session already
// existed (nothing was created) or when the window id could not be read.
func ensureSession(target string) (stray string, err error) {
	sessName := tmuxSessionName(target)
	if exec.Command("tmux", "has-session", "-t", "="+sessName).Run() == nil {
		return "", nil
	}
	out, err := exec.Command("tmux", "new-session", "-d", "-s", sessName, "-P", "-F", "#{window_id}").CombinedOutput()
	if err != nil {
		return "", tmuxCmdError("tmux new-session "+target, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// killStrayWindow removes the leftover default-shell window ensureSession left
// behind, once the real ax window is in place. Best-effort: a failure (the
// window already gone, a since-detached server) is not worth failing the launch
// over, since the stray is cosmetic. A "" id (session pre-existed) is a no-op.
func killStrayWindow(stray string) {
	if stray == "" {
		return
	}
	exec.Command("tmux", "kill-window", "-t", stray).Run()
}

// Locate matches the @ax_session tag, falling back to the pane's start command,
// which embeds the session id (e.g. "claude --resume <id>"). The fallback catches
// windows opened before tagging and is the more reliable signal.
func (tmux) Locate(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{window_id}\t#{@ax_session}\t#{pane_current_command}\t#{pane_start_command}").Output()
	if err != nil {
		return "", false
	}
	fallback := ""
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 4 {
			continue
		}
		if f[1] == sessionID && !staleTaggedPane(f[2], f[3], sessionID) {
			return f[0], true // the tag is authoritative
		}
		if fallback == "" && startCmdOwns(f[3], sessionID) {
			fallback = f[0]
		}
	}
	return fallback, fallback != ""
}

// startCmdOwns reports whether a pane's start command runs sessionID itself, as
// opposed to merely referencing it: a worker's command embeds its parent's id in
// AX_PARENT= (and its run's id in AX_RUN=, plus the deprecated AX_GROUP= kept
// alongside it), so a bare substring match would route the parent's
// send/kill/move into the worker's window.
func startCmdOwns(start, sessionID string) bool {
	idx := strings.Index(start, sessionID)
	for idx >= 0 {
		head := start[:idx]
		if !strings.HasSuffix(head, "AX_PARENT='") && !strings.HasSuffix(head, "AX_RUN='") && !strings.HasSuffix(head, "AX_GROUP='") {
			return true
		}
		next := strings.Index(start[idx+1:], sessionID)
		if next < 0 {
			return false
		}
		idx += 1 + next
	}
	return false
}

func staleTaggedPane(cmd, start, sessionID string) bool {
	if !isInteractiveShell(cmd) {
		return false
	}
	return !startCmdOwns(start, sessionID)
}

func isInteractiveShell(cmd string) bool {
	switch strings.ToLower(strings.TrimSpace(cmd)) {
	case "sh", "bash", "zsh", "fish", "pwsh", "powershell", "powershell.exe", "cmd", "cmd.exe", "nu", "xonsh":
		return true
	}
	return false
}

// Live builds session id -> "session:window.pane" by matching the @ax_session
// tag, falling back to the session id parsed out of the pane's start command
// (the same signals Locate uses).
func (tmux) Live() map[string]string {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{session_name}\t#{window_index}\t#{pane_index}\t#{@ax_session}\t#{pane_current_command}\t#{pane_start_command}").Output()
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.SplitN(line, "\t", 6)
		if len(f) < 6 {
			continue
		}
		id := strings.TrimSpace(f[3])
		if id == "" {
			id = sessionIDFromCmd(f[5])
		} else if staleTaggedPane(f[4], f[5], id) {
			continue
		}
		if id == "" {
			continue
		}
		if _, ok := m[id]; !ok {
			m[id] = f[0] + ":" + f[1] + "." + f[2]
		}
	}
	return m
}

// sessionIDFromCmd pulls the session id out of a start command, e.g.
// "claude --resume <id>", "pi --session <id>", or "claude --session-id <id>" (a
// hand-started session that carries its id). tmux may report the start command
// with surrounding quotes (e.g. when the whole "cd dir && pi --session <id>" is
// one quoted arg), which would otherwise leave a stray quote on the id, so trim
// quotes from the match.
func sessionIDFromCmd(cmd string) string {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		if (f == "--resume" || f == "--session" || f == "--session-id" || f == "resume") && i+1 < len(fields) {
			return strings.Trim(fields[i+1], `"'`)
		}
	}
	return ""
}

// Panes lists every open pane with its tag, start/current command, cwd, and pid,
// for correlating windows ax did not launch with the session running in them.
func (tmux) Panes() []Pane {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{session_name}\t#{window_index}\t#{pane_index}\t#{window_id}\t#{@ax_session}\t#{pane_current_command}\t#{pane_current_path}\t#{pane_pid}\t#{pane_start_command}").Output()
	if err != nil {
		return nil
	}
	var ps []Pane
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.SplitN(line, "\t", 9)
		if len(f) < 9 {
			continue
		}
		pid, _ := strconv.Atoi(f[7])
		ps = append(ps, Pane{
			Window:  f[3],
			Locator: f[0] + ":" + f[1] + "." + f[2],
			Tag:     strings.TrimSpace(f[4]),
			Cmd:     f[5],
			Cwd:     f[6],
			PID:     pid,
			Start:   f[8],
		})
	}
	return ps
}

func (tmux) Focus(window string) error {
	// switch-client -t <window_id> resolves the containing session, switches the
	// attached client there, and selects the window — works across sessions (the
	// mux_group regression) unlike select-window, which only selects within the
	// target's own session and leaves the client in place when they differ.
	// Without an attached client (TMUX unset) there is no client to switch, so
	// fall back to select-window which still updates the session's current window.
	if os.Getenv("TMUX") == "" {
		if out, err := exec.Command("tmux", "select-window", "-t", window).CombinedOutput(); err != nil {
			return tmuxCmdError("tmux select-window", err, out)
		}
		return nil
	}
	if out, err := exec.Command("tmux", switchClientArgs(window)...).CombinedOutput(); err != nil {
		return tmuxCmdError("tmux switch-client", err, out)
	}
	return nil
}

func switchClientArgs(target string) []string {
	args := []string{"switch-client"}
	if client := currentTmuxClient(); client != "" {
		args = append(args, "-c", client)
	}
	return append(args, "-t", target)
}

func currentTmuxClient() string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	// display-message runs in tmux's command context, so it can identify the
	// launching client even when several clients are attached to TMUX_PANE.
	if client := currentTmuxClientFromDisplayMessage(); client != "" {
		return client
	}
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return ""
	}
	out, err := exec.Command("tmux", "list-clients", "-F", "#{client_name}\t#{pane_id}").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.SplitN(line, "\t", 2)
		if len(f) == 2 && f[1] == pane {
			return f[0]
		}
	}
	return ""
}

func currentTmuxClientFromDisplayMessage() string {
	out, err := exec.Command("tmux", "display-message", "-p", "#{client_name}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Send types text into the window running sessionID. Multi-line text goes through
// a tmux paste buffer (bracketed paste) so a harness sees it as one paste, not
// per-line submits; short text uses literal send-keys. enter submits with a
// trailing Enter.
func (t tmux) Send(sessionID, text string, enter bool) error {
	w, ok := t.Locate(sessionID)
	if !ok {
		return fmt.Errorf("session %q not open in tmux", sessionID)
	}
	// "--" ends tmux's own flag parsing, so text starting with "-" (a bullet
	// list, "--continue") is delivered as input rather than read as options.
	if strings.Contains(text, "\n") {
		if err := exec.Command("tmux", "set-buffer", "-b", "axsend", "--", text).Run(); err != nil {
			return err
		}
		if err := exec.Command("tmux", "paste-buffer", "-d", "-p", "-b", "axsend", "-t", w).Run(); err != nil {
			return err
		}
	} else if text != "" {
		if err := exec.Command("tmux", "send-keys", "-t", w, "-l", "--", text).Run(); err != nil {
			return err
		}
	}
	if enter {
		if text != "" {
			time.Sleep(submitDelay) // let a burst-coalescing TUI (codex) settle before submit
		}
		return exec.Command("tmux", "send-keys", "-t", w, "Enter").Run()
	}
	return nil
}

// Interrupt sends ctrl-c into the window running sessionID.
func (t tmux) Interrupt(sessionID string) error {
	w, ok := t.Locate(sessionID)
	if !ok {
		return fmt.Errorf("session %q not open in tmux", sessionID)
	}
	return exec.Command("tmux", "send-keys", "-t", w, "C-c").Run()
}

// PaneTail returns the last `lines` lines of the pane running sessionID (empty
// when it is not open).
// MoveWindow relocates a session's window into the target tmux session, creating
// the session first when it does not exist ("=" pins has-session to an exact
// name). "target:" appends at the next free window index; -d keeps focus put.
// target is namespace-prefixed like any other session ax creates: a session by
// this exact unprefixed name from before this feature (or hand-created by the
// user) is left running untouched, but a move to the same name now lands in a
// freshly created "<prefix>target" alongside it rather than joining it.
func (t tmux) MoveWindow(sessionID, target string) error {
	win, ok := t.Locate(sessionID)
	if !ok {
		return fmt.Errorf("no window running %s", sessionID)
	}
	target = prefixName(target)
	stray, err := ensureSession(target)
	if err != nil {
		return err
	}
	if out, err := exec.Command("tmux", "move-window", "-d", "-s", win, "-t", tmuxSessionName(target)+":").CombinedOutput(); err != nil {
		return fmt.Errorf("move-window: %s", strings.TrimSpace(string(out)))
	}
	// The moved window now lives in the session beside ensureSession's initial
	// shell window; that shell is a stray now, so drop it.
	killStrayWindow(stray)
	return nil
}

// CloseWindow closes the tmux window running sessionID with `tmux kill-window`.
// For a dtach-held session (the default launch path) this only detaches: the
// held holder (ax run, wrapping the harness) keeps running on its dtach socket,
// so a later Open reattaches the same process rather than restarting it.
//
// Verified empirically on tmux 3.6b + dtach 0.9: killing a held window leaves
// the process alive (its heartbeat kept advancing while detached) and a fresh
// `dtach -a` reattaches it; an UNHELD window's process dies with kill-window. The
// caller is responsible for skipping unheld sessions (app.windowDetachSafe).
func (t tmux) CloseWindow(sessionID string) error {
	win, ok := t.Locate(sessionID)
	if !ok {
		return fmt.Errorf("no window running %s", sessionID)
	}
	if out, err := exec.Command("tmux", closeWindowArgs(win)...).CombinedOutput(); err != nil {
		return fmt.Errorf("kill-window: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// closeWindowArgs builds `tmux kill-window -t <window>`, targeting a specific
// window id so only that window is closed (never the caller's). Split out so the
// argv is unit-testable without a running tmux.
func closeWindowArgs(window string) []string {
	return []string{"kill-window", "-t", window}
}

func (t tmux) PaneTail(sessionID string, lines int) string {
	w, ok := t.Locate(sessionID)
	if !ok {
		return ""
	}
	out, err := exec.Command("tmux", "capture-pane", "-p", "-t", w, "-S", "-"+strconv.Itoa(lines)).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// Retag sets the current pane's @ax_session to sessionID, replacing the
// placeholder set when the window opened. It targets TMUX_PANE (the pane this
// process runs in) and is a no-op outside tmux.
func (tmux) Retag(sessionID string) error {
	pane := os.Getenv("TMUX_PANE")
	if os.Getenv("TMUX") == "" || pane == "" {
		return nil
	}
	return exec.Command("tmux", "set-option", "-w", "-t", pane, "@ax_session", sessionID).Run()
}
