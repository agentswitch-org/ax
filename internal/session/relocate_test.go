package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
)

func claudeStore(t *testing.T) (config.Harness, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "projects")
	return config.Harness{Name: "claude", Format: "claude", Glob: filepath.Join(root, "*", "*.jsonl")}, root
}

// A session stranded in the project folder of its old (pre-move) directory is
// moved to the folder claude derives from the new directory, sidecar included,
// so `claude --resume` finds it (the "No conversation found" crash).
func TestRelocateMovesStrandedTranscript(t *testing.T) {
	h, root := claudeStore(t)
	old := filepath.Join(root, "-Users-x-src-proj")
	if err := os.MkdirAll(filepath.Join(old, "abc"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(old, "abc.jsonl")
	os.WriteFile(file, []byte("{}\n"), 0o600)
	os.WriteFile(filepath.Join(old, "abc", "tool.json"), []byte("{}"), 0o600)

	s := Session{ID: "abc", File: file, Dir: "/Users/x/src/ax/proj"}
	if err := Relocate(h, s); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "-Users-x-src-ax-proj")
	if _, err := os.Stat(filepath.Join(want, "abc.jsonl")); err != nil {
		t.Fatalf("transcript not moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(want, "abc", "tool.json")); err != nil {
		t.Fatalf("sidecar not moved: %v", err)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("old transcript still present")
	}
	// A second call (stale index still pointing at the old path) is a no-op.
	if err := Relocate(h, s); err != nil {
		t.Fatalf("re-relocate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(want, "abc.jsonl")); err != nil {
		t.Fatalf("transcript gone after re-relocate: %v", err)
	}
}

// A transcript already in the right project folder is left alone.
func TestRelocateNoopWhenInPlace(t *testing.T) {
	h, root := claudeStore(t)
	dir := filepath.Join(root, "-Users-x-proj")
	os.MkdirAll(dir, 0o700)
	file := filepath.Join(dir, "abc.jsonl")
	os.WriteFile(file, []byte("{}\n"), 0o600)
	if err := Relocate(h, Session{ID: "abc", File: file, Dir: "/Users/x/proj"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("in-place transcript moved: %v", err)
	}
}

// Claude hashes mangled names past 200 chars with an algorithm we don't
// reproduce; those paths must error (logged upstream), not move to a wrong dir.
func TestRelocateRefusesOverlongPath(t *testing.T) {
	h, root := claudeStore(t)
	old := filepath.Join(root, "-short")
	os.MkdirAll(old, 0o700)
	file := filepath.Join(old, "abc.jsonl")
	os.WriteFile(file, []byte("{}\n"), 0o600)
	long := "/" + strings.Repeat("x", 220)
	if err := Relocate(h, Session{ID: "abc", File: file, Dir: long}); err == nil {
		t.Fatal("want error for overlong mangled path")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("transcript must stay put on refusal: %v", err)
	}
}

// Non-claude harnesses resolve sessions globally; Relocate must not touch them.
func TestRelocateIgnoresGlobalStores(t *testing.T) {
	h := config.Harness{Name: "pi", Format: "pi", Glob: "~/.pi/agent/sessions/*/*.jsonl"}
	if err := Relocate(h, Session{ID: "abc", File: "/nope/abc.jsonl", Dir: "/elsewhere"}); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeMangleMatchesClaude(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/x/src/agentswitch-org/ax": "-Users-x-src-agentswitch-org-ax",
		"/Users/x/.dotfiles":              "-Users-x--dotfiles",
		"/Users/x/a_b c":                  "-Users-x-a-b-c",
	} {
		if got, ok := claudeMangle(in); !ok || got != want {
			t.Fatalf("mangle(%q) = %q, want %q", in, got, want)
		}
	}
}
