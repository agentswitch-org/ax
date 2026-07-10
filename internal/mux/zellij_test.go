//go:build unix

package mux

import (
	"reflect"
	"testing"
)

func TestOpenTabArgs(t *testing.T) {
	got := openTabArgs("my title", "/proj")
	want := []string{"action", "new-tab", "--name", "my title", "--cwd", "/proj"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("openTabArgs = %v, want %v", got, want)
	}
	if got := openTabArgs("t", ""); reflect.DeepEqual(got, []string{"action", "new-tab", "--name", "t", "--cwd", ""}) {
		t.Errorf("openTabArgs must omit an empty --cwd, got %v", got)
	}
}

// runPaneArgs must route the command through a shell and put it after "--" so a
// command starting with "-" is delivered, not parsed as an option.
func TestRunPaneArgs(t *testing.T) {
	got := runPaneArgs("sid-1", "/proj", "claude --resume sid-1")
	want := []string{"run", "--in-place", "--close-on-exit", "--name", "sid-1", "--cwd", "/proj", "--", "sh", "-c", "claude --resume sid-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runPaneArgs = %v, want %v", got, want)
	}
}

func TestActionArgBuilders(t *testing.T) {
	if got, want := focusArgs("tab1"), []string{"action", "go-to-tab-name", "tab1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("focusArgs = %v, want %v", got, want)
	}
	if got, want := writeCharsArgs("-hi"), []string{"action", "write-chars", "--", "-hi"}; !reflect.DeepEqual(got, want) {
		t.Errorf("writeCharsArgs = %v, want %v", got, want)
	}
	if got, want := writeBytesArgs("3"), []string{"action", "write", "3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("writeBytesArgs = %v, want %v", got, want)
	}
	// zellij rejects the path as a bare positional argument (exit code 2); it
	// must be passed as --path, or PaneTail silently returns "" forever.
	if got, want := dumpScreenArgs("/tmp/x"), []string{"action", "dump-screen", "--path", "/tmp/x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dumpScreenArgs = %v, want %v", got, want)
	}
	if got, want := renamePaneArgs("3", "sid-1"), []string{"action", "rename-pane", "--pane-id", "3", "sid-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("renamePaneArgs = %v, want %v", got, want)
	}
}

func TestTailLines(t *testing.T) {
	if got := tailLines("a\nb\nc\nd\n", 2); got != "c\nd" {
		t.Errorf("tailLines = %q, want %q", got, "c\nd")
	}
	if got := tailLines("a\nb", 5); got != "a\nb" {
		t.Errorf("tailLines fewer-than-n = %q", got)
	}
}

const sampleLayout = `layout {
    tab name="claude·proj" focus=true {
        pane command="sh" name="abc-123" cwd="/home/x/proj" {
            args "-c" "claude --resume abc-123 --model opus"
        }
    }
    tab name="scratch" {
        pane
    }
}`

// parseLayout must lift each pane's name (the tag), fold its args into the start
// command (so sessionIDFromCmd can read the id), and record the enclosing tab as
// the focus handle and part of the locator.
func TestParseLayout(t *testing.T) {
	panes := parseLayout(sampleLayout, "sess")
	if len(panes) != 2 {
		t.Fatalf("got %d panes, want 2", len(panes))
	}
	p := panes[0]
	if p.Tag != "abc-123" {
		t.Errorf("Tag = %q, want abc-123", p.Tag)
	}
	if p.Window != "claude·proj" {
		t.Errorf("Window = %q, want the tab name", p.Window)
	}
	if p.Cwd != "/home/x/proj" {
		t.Errorf("Cwd = %q", p.Cwd)
	}
	if p.Locator != "sess:claude·proj.0" {
		t.Errorf("Locator = %q, want sess:claude·proj.0", p.Locator)
	}
	if want := "sh -c claude --resume abc-123 --model opus"; p.Start != want {
		t.Errorf("Start = %q, want %q", p.Start, want)
	}
	if id := sessionIDFromCmd(p.Start); id != "abc-123" {
		t.Errorf("sessionIDFromCmd(Start) = %q, want abc-123", id)
	}
}

// A zellij backend correlates a session to its tab by pane name first, then by
// the id embedded in the start command, mirroring tmux's tag/fallback pair.
func TestParseLayoutCorrelation(t *testing.T) {
	panes := parseLayout(sampleLayout, "sess")
	var byTag, byCmd string
	for _, p := range panes {
		if p.Tag == "abc-123" {
			byTag = p.Window
		}
		if startCmdOwns(p.Start, "abc-123") {
			byCmd = p.Window
		}
	}
	if byTag != "claude·proj" || byCmd != "claude·proj" {
		t.Errorf("correlation failed: byTag=%q byCmd=%q", byTag, byCmd)
	}
}
