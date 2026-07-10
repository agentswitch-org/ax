package adopt

import (
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/session"
)

func TestParseETime(t *testing.T) {
	cases := map[string]time.Duration{
		"05":         5 * time.Second,
		"01:02":      62 * time.Second,
		"01:02:03":   1*time.Hour + 2*time.Minute + 3*time.Second,
		"2-03:04:05": 2*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second,
		"":           -1,
		"junk":       -1,
		"1:2:3:4":    -1,
	}
	for in, want := range cases {
		if got := parseETime(in); got != want {
			t.Errorf("parseETime(%q)=%v want %v", in, got, want)
		}
	}
}

func TestMatch(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	paneStart := map[int]time.Time{
		10: base, // @90-like bare claude, opened at base
		11: base, // a second window, same dir
		12: base, // a shell (no harness)
	}
	procStart := func(pid int) time.Time { return paneStart[pid] }
	harnesses := map[string]bool{"claude": true, "pi": true}

	sessions := []session.Session{
		{ID: "old", Harness: "claude", Dir: "/proj", Created: base.Add(-time.Hour)}, // before window
		{ID: "mine", Harness: "claude", Dir: "/proj", Created: base.Add(1 * time.Minute)},
		{ID: "tagged", Harness: "claude", Dir: "/proj", Created: base.Add(2 * time.Minute)},
	}
	// "tagged" is already located on its own window; it must be excluded as a
	// candidate so the bare window doesn't grab it.
	located := map[string]string{"tagged": "s:9.1"}

	panes := []mux.Pane{
		{Window: "@90", Locator: "s:1.1", Tag: "", Start: "claude", Cmd: "claude", Cwd: "/proj", PID: 10},
		{Window: "@50", Locator: "s:9.1", Tag: "tagged", Start: "x", Cmd: "claude", Cwd: "/proj", PID: 11}, // resolved (tag)
		{Window: "@4", Locator: "s:2.1", Tag: "", Start: "zsh", Cmd: "zsh", Cwd: "/proj", PID: 12},         // not a harness
	}

	got := Match(panes, sessions, located, harnesses, procStart)
	if len(got) != 1 {
		t.Fatalf("want exactly one match, got %d: %v", len(got), got)
	}
	if p, ok := got["mine"]; !ok || p.Window != "@90" {
		t.Fatalf("want mine->@90, got %v", got)
	}
}

func TestMatchAbstainsWhenAmbiguous(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	procStart := func(int) time.Time { return base }
	harnesses := map[string]bool{"claude": true}
	// two sessions created after the window opened in the same dir: can't tell
	// which the window is running, so abstain.
	sessions := []session.Session{
		{ID: "a", Harness: "claude", Dir: "/proj", Created: base.Add(1 * time.Minute)},
		{ID: "b", Harness: "claude", Dir: "/proj", Created: base.Add(2 * time.Minute)},
	}
	panes := []mux.Pane{{Window: "@90", Locator: "s:1.1", Start: "claude", Cmd: "claude", Cwd: "/proj", PID: 10}}
	if got := Match(panes, sessions, map[string]string{}, harnesses, procStart); len(got) != 0 {
		t.Fatalf("want abstain (0), got %v", got)
	}
}

func TestMatchAbstainsWhenTwoPanesClaimOne(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	procStart := func(int) time.Time { return base }
	harnesses := map[string]bool{"claude": true}
	sessions := []session.Session{{ID: "only", Harness: "claude", Dir: "/proj", Created: base.Add(time.Minute)}}
	// two harness panes in the same dir, one session: ambiguous which pane runs it.
	panes := []mux.Pane{
		{Window: "@90", Locator: "s:1.1", Start: "claude", Cmd: "claude", Cwd: "/proj", PID: 10},
		{Window: "@91", Locator: "s:1.2", Start: "claude", Cmd: "claude", Cwd: "/proj", PID: 11},
	}
	if got := Match(panes, sessions, map[string]string{}, harnesses, procStart); len(got) != 0 {
		t.Fatalf("want abstain (0), got %v", got)
	}
}
