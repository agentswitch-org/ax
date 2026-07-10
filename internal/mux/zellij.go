package mux

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/agentswitch-org/ax/internal/shell"
)

// zellij drives Zellij through `zellij action ...` and `zellij run`. Each ax
// session lives in its own named tab: the tab name is the human title and the
// pane name is the session id, which stands in for tmux's @ax_session user
// option (Zellij exposes no per-pane user data over the CLI). Correlation is by
// pane name and, as with tmux, the session id embedded in the pane's start
// command (see sessionIDFromCmd), the more reliable of the two.
//
// Zellij is weaker than tmux at the query and targeting the interface needs, so
// several methods are best-effort. The specifics are called out on each method:
//   - Open always focuses (Zellij has no CLI "open in the background").
//   - Locate/Live/Panes read `zellij action dump-layout`, which covers only the
//     attached session, not every server-side session the way `tmux list-panes
//     -a` does.
//   - Send/Interrupt/PaneTail act on the focused pane, so they focus the target
//     tab first; a concurrent action can race the focus.
//   - Send has no bracketed-paste equivalent, so a multi-line send submits each
//     newline.
//   - MoveWindow has no CLI counterpart and returns an unsupported error.
type zellij struct{}

// Active reports whether we are inside a Zellij session (it exports ZELLIJ).
func (zellij) Active() bool     { return os.Getenv("ZELLIJ") != "" }
func (zellij) HasWindows() bool { return true }

// Open creates a tab named title and runs cmd in it, naming the pane sessionID
// so Locate can find it again. It runs cmd through `sh -c` (as tmux hands the
// command to a shell). focus is ignored: `zellij action new-tab` always focuses
// the new tab, and there is no CLI way to open one in the background. target is
// ignored: Zellij's CLI has no verb to open a tab in another session (the same
// reason MoveWindow is unsupported), so grouping collapses to flat tabs in the
// current session.
func (zellij) Open(dir, title, cmd, sessionID, target string, focus bool) error {
	_ = target
	if err := exec.Command("zellij", openTabArgs(prefixName(title), dir)...).Run(); err != nil {
		return err
	}
	if cmd == "" {
		return nil
	}
	// --in-place replaces the tab's fresh shell pane with the command pane, so the
	// tab holds a single named pane instead of a shell plus the command.
	return exec.Command("zellij", runPaneArgs(sessionID, dir, cmd)...).Run()
}

// Locate returns the tab running sessionID (matched by pane name, falling back
// to the session id in the pane's start command), for Focus/Send/Interrupt.
func (z zellij) Locate(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	for _, p := range z.Panes() {
		if p.Tag == sessionID {
			return p.Window, true // the pane name is authoritative
		}
	}
	for _, p := range z.Panes() {
		if startCmdOwns(p.Start, sessionID) {
			return p.Window, true
		}
	}
	return "", false
}

// Live maps each session id to a "session:tab.pane" locator for the picker's
// location column, using the same pane-name / start-command signals as Locate.
func (z zellij) Live() map[string]string {
	m := map[string]string{}
	for _, p := range z.Panes() {
		id := p.Tag
		if id == "" {
			id = sessionIDFromCmd(p.Start)
		}
		if id == "" {
			continue
		}
		if _, ok := m[id]; !ok {
			m[id] = p.Locator
		}
	}
	return m
}

// Panes lists every pane in the attached session (dump-layout's scope) with the
// data needed to correlate a window ax did not launch. PID is unavailable from
// the layout dump, so it is 0; the pane name carries the tag ax set at Open.
func (zellij) Panes() []Pane {
	out, err := exec.Command("zellij", "action", "dump-layout").Output()
	if err != nil {
		return nil
	}
	return parseLayout(string(out), os.Getenv("ZELLIJ_SESSION_NAME"))
}

// Focus switches to a tab by name.
func (zellij) Focus(window string) error {
	return exec.Command("zellij", focusArgs(window)...).Run()
}

// Send focuses the target tab, then types text into its pane; enter submits it.
// Unlike tmux there is no bracketed-paste path, so a multi-line text submits at
// each newline.
func (z zellij) Send(sessionID, text string, enter bool) error {
	tab, ok := z.Locate(sessionID)
	if !ok {
		return fmt.Errorf("session %q not open in zellij", sessionID)
	}
	if err := exec.Command("zellij", focusArgs(tab)...).Run(); err != nil {
		return err
	}
	if text != "" {
		if err := exec.Command("zellij", writeCharsArgs(text)...).Run(); err != nil {
			return err
		}
	}
	if enter {
		return exec.Command("zellij", writeBytesArgs("13")...).Run() // carriage return
	}
	return nil
}

// Interrupt focuses the target tab and writes ctrl-c (byte 3) into its pane.
func (z zellij) Interrupt(sessionID string) error {
	tab, ok := z.Locate(sessionID)
	if !ok {
		return fmt.Errorf("session %q not open in zellij", sessionID)
	}
	if err := exec.Command("zellij", focusArgs(tab)...).Run(); err != nil {
		return err
	}
	return exec.Command("zellij", writeBytesArgs("3")...).Run()
}

// PaneTail focuses the target tab, dumps its screen to a temp file (Zellij's
// dump-screen has no stdout form), and returns the last `lines` lines.
func (z zellij) PaneTail(sessionID string, lines int) string {
	tab, ok := z.Locate(sessionID)
	if !ok {
		return ""
	}
	if err := exec.Command("zellij", focusArgs(tab)...).Run(); err != nil {
		return ""
	}
	f, err := os.CreateTemp("", "ax-dump-*")
	if err != nil {
		return ""
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)
	if err := exec.Command("zellij", dumpScreenArgs(path)...).Run(); err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return tailLines(string(data), lines)
}

// MoveWindow is unsupported: Zellij has no CLI verb to move a tab into another
// session. Returned as an error (rather than a silent no-op) so `ax move`
// reports the skip instead of claiming success.
func (zellij) MoveWindow(sessionID, target string) error {
	return fmt.Errorf("zellij backend cannot move %s: moving a tab between sessions has no CLI verb", sessionID)
}

// CloseWindow closes the Zellij tab running sessionID. Zellij's `close-tab` acts
// on the focused tab (there is no --tab-name form), so it focuses the target tab
// first, then closes it. Like tmux kill-window, this only detaches a dtach-held
// pane (the held process survives on its socket); an unheld pane dies with the
// tab, so the caller skips unheld sessions (app.windowDetachSafe). The focus +
// close pair can race a concurrent Focus, the same best-effort caveat Send and
// PaneTail carry on this backend.
func (z zellij) CloseWindow(sessionID string) error {
	tab, ok := z.Locate(sessionID)
	if !ok {
		return fmt.Errorf("no tab running %s", sessionID)
	}
	if err := exec.Command("zellij", focusArgs(tab)...).Run(); err != nil {
		return err
	}
	return exec.Command("zellij", "action", "close-tab").Run()
}

// Retag renames the pane this process runs in to sessionID, mirroring
// tmux.Retag: `zellij action rename-pane` rewrites a pane's name attribute,
// the exact field parseLayout reads as Tag, so a late-revealed session id
// (e.g. after --resume) is reflected the same way a fresh Open's --name is.
//
// It targets the pane by ZELLIJ_PANE_ID rather than focusing and renaming the
// focused pane: Zellij sets this env var per-pane (Zellij's analogue of
// tmux's TMUX_PANE), and `rename-pane --pane-id` accepts it directly, so the
// call is exact even if another pane is focused when this runs. It is a
// no-op outside Zellij.
func (zellij) Retag(sessionID string) error {
	pane := os.Getenv("ZELLIJ_PANE_ID")
	if os.Getenv("ZELLIJ") == "" || pane == "" {
		return nil
	}
	return exec.Command("zellij", renamePaneArgs(pane, sessionID)...).Run()
}

// openTabArgs builds `zellij action new-tab --name <title> [--cwd <dir>]`.
func openTabArgs(title, dir string) []string {
	args := []string{"action", "new-tab"}
	if title != "" {
		args = append(args, "--name", title)
	}
	if dir != "" {
		args = append(args, "--cwd", dir)
	}
	return args
}

// runPaneArgs builds `zellij run --in-place --close-on-exit [--name <id>]
// [--cwd <dir>] -- sh -c <cmd>`, running the command through a shell as tmux
// does. "--" ends flag parsing so a command starting with "-" is not read as an
// option.
//
// Unlike tmux's spillLongCmd path, cmd is never spilled to a temp script here:
// tmux's new-window cap comes from its own client-server command message
// (~16 KB), but zellij's client forwards run's argv over a protobuf IPC
// message with no comparable small buffer. Verified empirically: a 120 KB cmd
// (well past tmux's cap and the coordinator.md-scale 23 KB regression) round
// trips through `zellij run ... -- sh -c <cmd>` byte-for-byte.
func runPaneArgs(sessionID, dir, cmd string) []string {
	args := []string{"run", "--in-place", "--close-on-exit"}
	if sessionID != "" {
		args = append(args, "--name", sessionID)
	}
	if dir != "" {
		args = append(args, "--cwd", dir)
	}
	// "--" ends zellij's flag parsing, then the platform shell runs the command.
	tail := append([]string{"--"}, shell.Prefix()...)
	return append(args, append(tail, cmd)...)
}

// focusArgs builds `zellij action go-to-tab-name <tab>`.
func focusArgs(tab string) []string {
	return []string{"action", "go-to-tab-name", tab}
}

// writeCharsArgs builds `zellij action write-chars -- <text>`.
func writeCharsArgs(text string) []string {
	return []string{"action", "write-chars", "--", text}
}

// writeBytesArgs builds `zellij action write <byte>...` (decimal byte values).
func writeBytesArgs(bytes ...string) []string {
	return append([]string{"action", "write"}, bytes...)
}

// dumpScreenArgs builds `zellij action dump-screen --path <path>`. The path
// must be passed as a flag: zellij rejects it as a bare positional argument
// (exit code 2).
func dumpScreenArgs(path string) []string {
	return []string{"action", "dump-screen", "--path", path}
}

// renamePaneArgs builds `zellij action rename-pane --pane-id <paneID> <name>`.
func renamePaneArgs(paneID, name string) []string {
	return []string{"action", "rename-pane", "--pane-id", paneID, name}
}

// tailLines returns the last n lines of s.
func tailLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

var (
	kdlTabRe      = regexp.MustCompile(`^\s*tab\b`)
	kdlPaneRe     = regexp.MustCompile(`^\s*pane\b`)
	kdlAttrRe     = regexp.MustCompile(`(\w+)="([^"]*)"`)
	kdlArgsRe     = regexp.MustCompile(`^\s*args\b(.*)$`)
	kdlQuoted     = regexp.MustCompile(`"([^"]*)"`)
	kdlTemplateRe = regexp.MustCompile(`^\s*(new_tab_template|swap_tiled_layout|swap_floating_layout)\b`)
)

// parseLayout turns `zellij action dump-layout` KDL into panes. The dump is the
// only CLI window into a session's tabs and panes, so this is a line-oriented
// best-effort parse: it tracks the enclosing tab name, reads each pane's name /
// command / cwd attributes, and folds any following `args "..."` child into the
// pane's start command (so sessionIDFromCmd / startCmdOwns can read the id out
// of it, exactly as with tmux's pane_start_command). session names the attached
// Zellij session for the "session:tab.pane" locator.
//
// After the live tabs, the dump appends a new_tab_template and one or more
// swap_tiled_layout / swap_floating_layout blocks: canned alternate-layout
// definitions that reuse the same "tab"/"pane" KDL nodes as the live layout.
// zellij always emits these after the real tabs, so parsing stops at the first
// one rather than tracking brace depth to skip over them.
func parseLayout(kdl, session string) []Pane {
	var panes []Pane
	tab := ""
	paneIdx := 0
	cur := -1 // index into panes of the pane currently accepting args children
	for _, line := range strings.Split(kdl, "\n") {
		if kdlTemplateRe.MatchString(line) {
			break
		}
		switch {
		case kdlTabRe.MatchString(line):
			tab = attr(line, "name")
			paneIdx = 0
			cur = -1
		case kdlPaneRe.MatchString(line):
			name := attr(line, "name")
			cmd := attr(line, "command")
			locator := session + ":" + tab + "." + strconv.Itoa(paneIdx)
			panes = append(panes, Pane{
				Window:  tab,
				Locator: locator,
				Tag:     name,
				Cmd:     cmd,
				Cwd:     attr(line, "cwd"),
				Start:   cmd,
			})
			cur = len(panes) - 1
			paneIdx++
		default:
			if m := kdlArgsRe.FindStringSubmatch(line); m != nil && cur >= 0 {
				var parts []string
				for _, q := range kdlQuoted.FindAllStringSubmatch(m[1], -1) {
					parts = append(parts, q[1])
				}
				if len(parts) > 0 {
					panes[cur].Start = strings.TrimSpace(panes[cur].Start + " " + strings.Join(parts, " "))
				}
			}
		}
	}
	return panes
}

// attr reads a KDL string attribute (key="value") off a line.
func attr(line, key string) string {
	for _, m := range kdlAttrRe.FindAllStringSubmatch(line, -1) {
		if m[1] == key {
			return m[2]
		}
	}
	return ""
}
