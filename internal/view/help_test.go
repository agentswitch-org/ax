package view

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/keys"
)

// helpBoxLines returns the framed box lines (those bounded by the vertical
// borders), each with its left indent and trailing newline removed.
func helpBoxLines(out string) []string {
	var box []string
	for _, l := range strings.Split(out, "\n") {
		s := StripANSI(l)
		t := strings.TrimLeft(s, " ")
		if strings.HasPrefix(t, "║") || strings.HasPrefix(t, "╔") ||
			strings.HasPrefix(t, "╚") || strings.HasPrefix(t, "╠") {
			box = append(box, t)
		}
	}
	return box
}

// TestHelpFrameFits checks that at a range of terminal widths every framed row
// has the same display width (so the right border is flush) and the box never
// exceeds the terminal width.
func TestHelpFrameFits(t *testing.T) {
	km := keys.Build(nil)
	// The key column can't shrink below the widest key label, so the box has a
	// hard minimum width. Above it the box must fit the terminal; below it (a
	// terminal too narrow for a legible help) the frame must still be flush.
	for _, cols := range []int{200, 160, 120, 100, 90, 70, 64, 50, 40} {
		out := Help(km, "Ctrl-A then D", 60, cols)
		box := helpBoxLines(out)
		if len(box) == 0 {
			t.Fatalf("cols=%d: no box rendered", cols)
		}
		want := dispWidth(box[0])
		for i, l := range box {
			if w := dispWidth(l); w != want {
				t.Errorf("cols=%d line %d width %d, want %d (ragged frame): %q", cols, i, w, want, l)
			}
		}
		if cols >= 64 && want > cols {
			t.Errorf("cols=%d: box width %d exceeds terminal width", cols, want)
		}
	}
}

// TestHelpShowsEveryBinding checks that every configurable action with at least
// one resolved key has its keys and description in the wide render, plus the
// structural rows. Keyless actions (New/NewArgs, reachable only through the
// compose flow) are intentionally omitted so no blank-key rows render.
func TestHelpShowsEveryBinding(t *testing.T) {
	km := keys.Build(nil)
	out := StripANSI(Help(km, "Ctrl-A then D", 60, 200))
	for _, d := range keys.Defs {
		if len(km.Keys(d.Action)) == 0 {
			if strings.Contains(out, d.Desc) {
				t.Errorf("keyless action %s should not render a row, but %q appears", d.Action, d.Desc)
			}
			continue
		}
		if !strings.Contains(out, d.Desc) {
			t.Errorf("missing description for %s: %q", d.Action, d.Desc)
		}
		for _, k := range km.Keys(d.Action) {
			if !strings.Contains(out, k) {
				t.Errorf("missing key %q for %s", k, d.Action)
			}
		}
	}
	for _, s := range []string{"enter", "esc", "ctrl-a then d", "MOVE", "PREVIEW", "SESSION", "QUIT",
		"agentswitch", "support@agentswitch.org"} {
		if !strings.Contains(out, s) {
			t.Errorf("expected %q in help output", s)
		}
	}
}

// TestHelpKeyColumnAligned checks that in the wide render every description in a
// panel starts at the same column, so no row (such as the open-windows row) is
// ragged relative to its neighbours.
func TestHelpKeyColumnAligned(t *testing.T) {
	km := keys.Build(nil)
	out := Help(km, "Ctrl-A then D", 60, 200)
	// Sample a few key rows that share the single-letter key column and confirm
	// their descriptions align. "o" (open windows) must match the others.
	probes := map[string]string{
		"detach the attached window": "w",
		"open a window and reattach": "o",
		"resume the session with no": "e",
		"kill the session, stopping": "x",
	}
	col := -1
	for _, l := range strings.Split(StripANSI(out), "\n") {
		for desc := range probes {
			if idx := strings.Index(l, desc); idx >= 0 {
				if col == -1 {
					col = idx
				} else if idx != col {
					t.Errorf("description %q starts at col %d, want %d (ragged): %q", desc, idx, col, l)
				}
			}
		}
	}
	if col == -1 {
		t.Fatal("probe descriptions not found")
	}
}

// TestHelpNarrowDropsBanner checks the banner is dropped (not overflowed) when
// the terminal is too narrow to hold it.
func TestHelpNarrowDropsBanner(t *testing.T) {
	km := keys.Build(nil)
	narrow := StripANSI(Help(km, "Ctrl-A then D", 60, 70))
	if strings.Contains(narrow, "agentswitch  ·  v") {
		t.Error("banner subtitle should be dropped on a narrow terminal")
	}
	wide := StripANSI(Help(km, "Ctrl-A then D", 60, 200))
	if !strings.Contains(wide, "agentswitch  ·  v") {
		t.Error("banner subtitle should be present on a wide terminal")
	}
}
