package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/session"
)

func TestFindNewSession(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	sessions := []session.Session{
		{Harness: "codex", ID: "old", Dir: "/proj", Last: base},
		{Harness: "codex", ID: "new1", Dir: "/proj", Last: base.Add(1 * time.Minute)},
		{Harness: "codex", ID: "new2", Dir: "/proj", Last: base.Add(2 * time.Minute)}, // newest new
		{Harness: "codex", ID: "elsewhere", Dir: "/other", Last: base.Add(9 * time.Minute)},
		{Harness: "pi", ID: "wrongharness", Dir: "/proj", Last: base.Add(9 * time.Minute)},
	}
	before := map[string]bool{"old": true}

	// newest not-before session of this harness in this dir wins; a newer session
	// in another dir and another harness are both ignored.
	if got := findNewSession(sessions, "codex", "/proj", before); got != "new2" {
		t.Fatalf("want new2, got %q", got)
	}
	// none new yet
	allSeen := map[string]bool{"old": true, "new1": true, "new2": true}
	if got := findNewSession(sessions, "codex", "/proj", allSeen); got != "" {
		t.Fatalf("want empty when nothing new, got %q", got)
	}
	// unknown launch dir: dir filter is skipped, so the newest new codex wins
	// regardless of directory.
	if got := findNewSession(sessions, "codex", "", before); got != "elsewhere" {
		t.Fatalf("want elsewhere when dir unknown, got %q", got)
	}
	// a session with no recorded dir is accepted even when the launch dir is known.
	nodir := []session.Session{{Harness: "codex", ID: "nd", Dir: "", Last: base}}
	if got := findNewSession(nodir, "codex", "/proj", map[string]bool{}); got != "nd" {
		t.Fatalf("want nd when session dir empty, got %q", got)
	}
}

// The launch dir and the harness-recorded dir can be two spellings of the same
// place: os.Getwd() in the adopt wrapper returns $PWD verbatim (e.g. the
// unresolved /tmp/x) while codex records the symlink-resolved cwd (/private/tmp/x
// on macOS). findNewSession must resolve both before comparing, or codex adoption
// silently times out and its session is never bound to the launch id.
func TestFindNewSessionResolvesSymlinkedDir(t *testing.T) {
	real := t.TempDir()
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linkdir")
	if err := os.Symlink(resolved, link); err != nil {
		// Windows needs elevation or Developer Mode to create a symlink; without
		// it there is nothing to exercise, so skip rather than fail.
		t.Skipf("cannot create symlink: %v", err)
	}

	base := time.Unix(1_700_000_000, 0)
	// The session records the resolved path; the launch (adopt) dir is the symlink.
	sessions := []session.Session{{Harness: "codex", ID: "cx", Dir: resolved, Last: base}}
	if got := findNewSession(sessions, "codex", link, map[string]bool{}); got != "cx" {
		t.Fatalf("symlinked launch dir must still match the resolved session dir, got %q", got)
	}
}

func TestHarnessIDs(t *testing.T) {
	sessions := []session.Session{
		{Harness: "codex", ID: "a"},
		{Harness: "codex", ID: "b"},
		{Harness: "pi", ID: "c"},
	}
	ids := harnessIDs(sessions, "codex")
	if len(ids) != 2 || !ids["a"] || !ids["b"] || ids["c"] {
		t.Fatalf("want {a,b}, got %v", ids)
	}
}
