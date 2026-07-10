package view

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/session"
)

// winTarget turns a raw tmux locator ("session:window.pane") into the switch
// target the user types ("session:window"), dropping the pane and preserving
// the ":window" index when the session name would otherwise push it out of the
// column.
func TestWinTarget(t *testing.T) {
	cases := []struct {
		name string
		loc  string
		w    int
		want string
	}{
		{"empty", "", 14, ""},
		{"pane dropped", "main:5.0", 14, "main:5"},
		{"pane dropped, multi-digit window", "work:12.1", 14, "work:12"},
		{"run session fits", "ax_812d2aed:0.0", 14, "ax_812d2aed:0"},
		{"long session trims left, keeps index", "ax_agentswitch:1.0", 14, "…agentswitch:1"},
		{"no pane suffix", "main:3", 14, "main:3"},
		{"index survives even in a narrow column", "ax_agentswitch:7.0", 6, "…tch:7"},
	}
	for _, c := range cases {
		if got := winTarget(c.loc, c.w); got != c.want {
			t.Errorf("%s: winTarget(%q, %d) = %q, want %q", c.name, c.loc, c.w, got, c.want)
		}
		if dispWidth(winTarget(c.loc, c.w)) > c.w {
			t.Errorf("%s: winTarget(%q, %d) = %q overflows width %d", c.name, c.loc, c.w, winTarget(c.loc, c.w), c.w)
		}
	}
}

// The WIN column renders the jumpable target from a session's live locator, so
// a row names the same "session:window" tmux does.
func TestWinColumnRendersJumpTarget(t *testing.T) {
	col := columnByKey(t, "win")
	got := StripANSI(col.cell(session.Session{}, nil, RowMeta{Locator: "ax_812d2aed:2.0"}, col.width, 0))
	if got != "ax_812d2aed:2" {
		t.Fatalf("WIN cell = %q, want %q", got, "ax_812d2aed:2")
	}
}

func columnByKey(t *testing.T, key string) column {
	t.Helper()
	for _, c := range registry {
		if c.key == key {
			return c
		}
	}
	t.Fatalf("no %q column in registry", key)
	return column{}
}
