package mux

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// startCmdOwns must not mistake a worker's env references to its parent's or
// run's id (AX_PARENT=/AX_RUN=/the deprecated AX_GROUP=) for the command
// actually running that id.
func TestStartCmdOwnsIgnoresEnvReferences(t *testing.T) {
	const id = "abc-123"
	cases := []struct {
		name  string
		start string
		want  bool
	}{
		{"runs it", "sh -c claude --session-id " + id, true},
		{"parent ref only", "AX_PARENT='" + id + "' sh -c claude --session-id newid", false},
		{"run ref only", "AX_RUN='" + id + "' sh -c claude --session-id newid", false},
		{"deprecated group ref only", "AX_GROUP='" + id + "' sh -c claude --session-id newid", false},
		{"run and group refs, then runs it", "AX_RUN='" + id + "' AX_GROUP='" + id + "' sh -c claude --session-id " + id, true},
	}
	for _, c := range cases {
		if got := startCmdOwns(c.start, id); got != c.want {
			t.Errorf("%s: startCmdOwns(%q, %q) = %v, want %v", c.name, c.start, id, got, c.want)
		}
	}
}

func TestStaleTaggedPane(t *testing.T) {
	const id = "abc-123"
	cases := []struct {
		name  string
		cmd   string
		start string
		want  bool
	}{
		{"shell prompt with no owner", "zsh", "", true},
		{"shell prompt with parent env only", "bash", "AX_PARENT='" + id + "' sh -c claude --session-id child", true},
		{"shell wrapper still owns session", "sh", "sh -c claude --session-id " + id, false},
		{"attach client still running", "ax", "", false},
		{"harness still running", "claude", "", false},
	}
	for _, c := range cases {
		if got := staleTaggedPane(c.cmd, c.start, id); got != c.want {
			t.Errorf("%s: staleTaggedPane(%q, %q, %q) = %v, want %v", c.name, c.cmd, c.start, id, got, c.want)
		}
	}
}

// New reads the `mux` setting from config on disk and returns that backend.
func TestNewPicksBackendFromConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`mux = "zellij"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)

	if _, ok := New().(zellij); !ok {
		t.Fatalf("New() = %T, want zellij", New())
	}
}

func TestEffectiveNameReportsPlatformDefault(t *testing.T) {
	cases := []struct {
		name string
		goos string
		want string
	}{
		{"", "darwin", "tmux"},
		{"unknown", "linux", "tmux"},
		{"", "windows", "process"},
		{"unknown", "windows", "process"},
		{"tmux", "windows", "tmux"},
		{"zellij", "windows", "zellij"},
		{"process", "darwin", "process"},
		{"none", "windows", "none"},
	}
	for _, tc := range cases {
		if got := effectiveName(tc.name, tc.goos); got != tc.want {
			t.Errorf("effectiveName(%q, %q) = %q, want %q", tc.name, tc.goos, got, tc.want)
		}
	}
}

// resolvePrefix defaults to "ax:", "off" disables it, and anything else is a
// literal override.
func TestResolvePrefix(t *testing.T) {
	cases := map[string]string{
		"":      "ax:",
		"off":   "",
		"myax:": "myax:",
		"[ax] ": "[ax] ",
	}
	for raw, want := range cases {
		if got := resolvePrefix(raw); got != want {
			t.Errorf("resolvePrefix(%q) = %q, want %q", raw, got, want)
		}
	}
}

// prefixName stamps the configured prefix on a nonempty name, leaves an empty
// name alone, and does not stack the prefix on a name that already carries it.
func TestPrefixName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("AX_CONFIG", path)

	if got := prefixName("proj·claude"); got != "ax:proj·claude" {
		t.Errorf("prefixName = %q, want ax:proj·claude", got)
	}
	if got := prefixName(""); got != "" {
		t.Errorf("prefixName(\"\") = %q, want empty", got)
	}
	if got := prefixName("ax:proj·claude"); got != "ax:proj·claude" {
		t.Errorf("prefixName must not stack: got %q", got)
	}

	if err := os.WriteFile(path, []byte(`mux_prefix = "off"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := prefixName("proj·claude"); got != "proj·claude" {
		t.Errorf("prefixName with mux_prefix=off = %q, want unprefixed", got)
	}

	if err := os.WriteFile(path, []byte(`mux_prefix = "team:"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := prefixName("proj·claude"); got != "team:proj·claude" {
		t.Errorf("prefixName with custom mux_prefix = %q, want team:proj·claude", got)
	}
}

// windowArgs builds the tmux new-window argv; -d only appears when not
// focusing, -c only when a dir is given, and -t only when a target session is
// named (grouping on). An empty target keeps the flat current-session argv.
func TestWindowArgs(t *testing.T) {
	got := windowArgs("/proj", "ax:proj·claude", "claude --resume x", "", true)
	want := []string{"new-window", "-P", "-F", "#{window_id}", "-n", "ax:proj·claude", "-c", "/proj", "claude --resume x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("windowArgs = %v, want %v", got, want)
	}
	got = windowArgs("", "ax:proj·claude", "claude", "", false)
	want = []string{"new-window", "-P", "-F", "#{window_id}", "-d", "-n", "ax:proj·claude", "claude"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("windowArgs (background, no dir) = %v, want %v", got, want)
	}
	// Grouping on: a named target adds -t <sanitized>: so the window is born
	// there. tmuxSessionName converts colons to underscores before the colon
	// that tmux uses for the window-index separator.
	got = windowArgs("/proj", "ax:proj·claude", "claude", "ax:proj", true)
	want = []string{"new-window", "-P", "-F", "#{window_id}", "-n", "ax:proj·claude", "-c", "/proj", "-t", "ax_proj:", "claude"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("windowArgs (grouped) = %v, want %v", got, want)
	}
}

func TestTmuxOpenRejectsEmptyCommand(t *testing.T) {
	err := (tmux{}).Open("", "title", " \t", "sid", "", true)
	if err == nil || !strings.Contains(err.Error(), "empty tmux command") {
		t.Fatalf("tmux.Open empty command = %v, want refusal", err)
	}
}

func TestTmuxGroupedEnsureSessionIncludesStderr(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	t.Setenv("TMUX_FAKE_FAIL_NEW_SESSION", "1")

	err := (tmux{}).Open("", "title", "claude --resume x", "sid", "group", true)
	if err == nil {
		t.Fatal("tmux.Open grouped with failing new-session = nil, want error")
	}
	if msg := err.Error(); !strings.Contains(msg, "tmux new-session") || !strings.Contains(msg, "new-session stderr detail") {
		t.Fatalf("tmux.Open grouped new-session error = %q, want stderr detail", msg)
	}
}

func TestTmuxGroupedFocusFailureIncludesStderr(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	t.Setenv("TMUX_FAKE_HAS_SESSION", "1")
	t.Setenv("TMUX_FAKE_FAIL_SWITCH", "1")

	err := (tmux{}).Open("", "title", "claude --resume x", "sid", "group", true)
	if err == nil {
		t.Fatal("tmux.Open grouped with failing switch-client = nil, want error")
	}
	if msg := err.Error(); !strings.Contains(msg, "tmux switch-client") || !strings.Contains(msg, "switch-client stderr detail") {
		t.Fatalf("tmux.Open grouped switch-client error = %q, want stderr detail", msg)
	}
}

func TestTmuxGroupedFocusSwitchesToNewWindow(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	t.Setenv("TMUX_FAKE_HAS_SESSION", "1")
	log := filepath.Join(t.TempDir(), "tmux.log")
	t.Setenv("TMUX_FAKE_LOG", log)

	err := (tmux{}).Open("", "title", "claude --resume x", "sid", "group", true)
	if err != nil {
		t.Fatalf("tmux.Open grouped focus = %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	calls := string(data)
	if !strings.Contains(calls, "switch-client -t @123\n") {
		t.Fatalf("tmux calls = %q, want switch-client to target new window @123", calls)
	}
	if strings.Contains(calls, "switch-client -t ax_group\n") {
		t.Fatalf("tmux calls = %q, must not switch only to the target session", calls)
	}
}

func TestTmuxGroupedFocusPinsCurrentPaneClient(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	t.Setenv("TMUX", "/tmp/tmux-test,1,0")
	t.Setenv("TMUX_PANE", "%42")
	t.Setenv("TMUX_FAKE_FAIL_DISPLAY", "1")
	t.Setenv("TMUX_FAKE_HAS_SESSION", "1")
	t.Setenv("TMUX_FAKE_CLIENTS", "/dev/pts/7\t%42\n/dev/pts/8\t%99\n")
	log := filepath.Join(t.TempDir(), "tmux.log")
	t.Setenv("TMUX_FAKE_LOG", log)

	err := (tmux{}).Open("", "title", "claude --resume x", "sid", "group", true)
	if err != nil {
		t.Fatalf("tmux.Open grouped focus = %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	calls := string(data)
	if !strings.Contains(calls, "switch-client -c /dev/pts/7 -t @123\n") {
		t.Fatalf("tmux calls = %q, want switch-client pinned to /dev/pts/7", calls)
	}
}

func TestTmuxGroupedFocusPinsDisplayMessageClientWithSamePaneClients(t *testing.T) {
	installFakeTmux(t)
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	t.Setenv("TMUX", "/tmp/tmux-test,1,0")
	t.Setenv("TMUX_PANE", "%42")
	t.Setenv("TMUX_FAKE_HAS_SESSION", "1")
	t.Setenv("TMUX_FAKE_CURRENT_CLIENT", "/dev/pts/8")
	t.Setenv("TMUX_FAKE_CLIENTS", "/dev/pts/7\t%42\n/dev/pts/8\t%42\n")
	log := filepath.Join(t.TempDir(), "tmux.log")
	t.Setenv("TMUX_FAKE_LOG", log)

	err := (tmux{}).Open("", "title", "claude --resume x", "sid", "group", true)
	if err != nil {
		t.Fatalf("tmux.Open grouped focus = %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	calls := string(data)
	if !strings.Contains(calls, "display-message -p #{client_name}\n") {
		t.Fatalf("tmux calls = %q, want display-message current-client probe", calls)
	}
	if !strings.Contains(calls, "switch-client -c /dev/pts/8 -t @123\n") {
		t.Fatalf("tmux calls = %q, want switch-client pinned to display-message client /dev/pts/8", calls)
	}
	if strings.Contains(calls, "switch-client -c /dev/pts/7 -t @123\n") {
		t.Fatalf("tmux calls = %q, must not choose the first same-pane client", calls)
	}
}

func installFakeTmux(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tmux helper uses a POSIX shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
if [ -n "$TMUX_FAKE_LOG" ]; then
	printf '%s\n' "$*" >> "$TMUX_FAKE_LOG"
fi
case "$1" in
has-session)
	if [ "$TMUX_FAKE_HAS_SESSION" = "1" ]; then
		exit 0
	fi
	exit 1
	;;
new-session)
	if [ "$TMUX_FAKE_FAIL_NEW_SESSION" = "1" ]; then
		echo "new-session stderr detail" >&2
		exit 42
	fi
	exit 0
	;;
new-window)
	echo "@123"
	exit 0
	;;
list-clients)
	if [ -n "$TMUX_FAKE_CLIENTS" ]; then
		printf '%b' "$TMUX_FAKE_CLIENTS"
	fi
	exit 0
	;;
display-message)
	if [ "$TMUX_FAKE_FAIL_DISPLAY" = "1" ]; then
		exit 44
	fi
	if [ -n "$TMUX_FAKE_CURRENT_CLIENT" ]; then
		printf '%s\n' "$TMUX_FAKE_CURRENT_CLIENT"
	fi
	exit 0
	;;
set-option)
	exit 0
	;;
switch-client)
	if [ "$TMUX_FAKE_FAIL_SWITCH" = "1" ]; then
		echo "switch-client stderr detail" >&2
		exit 43
	fi
	exit 0
	;;
*)
	exit 0
	;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// closeWindowArgs targets kill-window at a specific window id, so closing a
// held session's window detaches only that window and never the caller's.
func TestCloseWindowArgs(t *testing.T) {
	got := closeWindowArgs("@7")
	want := []string{"kill-window", "-t", "@7"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("closeWindowArgs = %v, want %v", got, want)
	}
}

// The process backend has no window to close, so CloseWindow reports an error
// rather than implying a detach that did not happen; none stays a pure no-op
// like its other methods.
func TestCloseWindowUnsupportedBackends(t *testing.T) {
	if err := (none{}).CloseWindow("id"); err != nil {
		t.Errorf("none.CloseWindow = %v, want nil (no-op)", err)
	}
	if err := (process{}).CloseWindow("id"); err == nil {
		t.Error("process.CloseWindow = nil, want an error (a subprocess has no window)")
	}
}

// A backend that cannot place a window into a named session ignores a non-empty
// target and collapses to its flat behavior. none is the clean case: Open is a
// no-op whether or not a target is passed, so grouping never changes its result.
func TestNoneOpenIgnoresTarget(t *testing.T) {
	if err := (none{}).Open("/proj", "title", "cmd", "id", "ax:proj", true); err != nil {
		t.Errorf("none.Open with a target = %v, want nil (no-op collapse to flat)", err)
	}
	if err := (none{}).Open("/proj", "title", "cmd", "id", "", true); err != nil {
		t.Errorf("none.Open without a target = %v, want nil", err)
	}
}

// tmux 3.6b replaces colons and dots with underscores in session names at
// creation time; tmuxSessionName must produce the same result so has-session,
// new-window -t, switch-client, and move-window -t agree with what was stored.
func TestTmuxSessionName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"ax:agentswitch", "ax_agentswitch"}, // default prefix + colon
		{"ax.foo", "ax_foo"},                 // dot
		{"ax:foo.bar", "ax_foo_bar"},         // colon and dot
		{"ax:proj·claude", "ax_proj·claude"}, // middle-dot U+00B7 is not ASCII dot
		{"normal", "normal"},                 // no special chars
		{"with space", "with space"},         // space not sanitized by tmux
	}
	for _, c := range cases {
		if got := tmuxSessionName(c.in); got != c.want {
			t.Errorf("tmuxSessionName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
