package search

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
)

// writeFile creates a temp file with the given content and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	return p
}

// --- inproc tests ---

func TestInprocEmptyQuery(t *testing.T) {
	got := inproc{}.Matches("", []string{"anyfile"}, 0)
	if got != nil {
		t.Errorf("empty query should return nil, got %v", got)
	}
}

func TestInprocEmptyFiles(t *testing.T) {
	got := inproc{}.Matches("foo", nil, 0)
	if got != nil {
		t.Errorf("empty files should return nil, got %v", got)
	}
}

func TestInprocWhitespaceOnlyQuery(t *testing.T) {
	got := inproc{}.Matches("   ", []string{"anyfile"}, 0)
	if got != nil {
		t.Errorf("whitespace-only query should return nil, got %v", got)
	}
}

func TestInprocBasicMatch(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "a.txt", "hello world\nfoo bar\nbaz\n")
	got := inproc{}.Matches("foo", []string{f}, 0)
	if lines, ok := got[f]; !ok {
		t.Fatalf("file should have a match")
	} else if len(lines) != 1 || lines[0] != 2 {
		t.Errorf("want line [2], got %v", lines)
	}
}

func TestInprocCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "b.txt", "Hello World\nFOO BAR\nbaz\n")
	got := inproc{}.Matches("foo", []string{f}, 0)
	if lines, ok := got[f]; !ok {
		t.Fatalf("case-insensitive match should find FOO BAR")
	} else if len(lines) != 1 || lines[0] != 2 {
		t.Errorf("want line [2], got %v", lines)
	}
}

func TestInprocMultipleMatchesInFile(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "c.txt", "foo one\nbar\nfoo two\n")
	got := inproc{}.Matches("foo", []string{f}, 0)
	if lines, ok := got[f]; !ok {
		t.Fatalf("expected matches")
	} else if len(lines) != 2 || lines[0] != 1 || lines[1] != 3 {
		t.Errorf("want lines [1, 3], got %v", lines)
	}
}

func TestInprocPerFileCap(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "d.txt", "foo\nfoo\nfoo\nfoo\n")
	got := inproc{}.Matches("foo", []string{f}, 2)
	if lines, ok := got[f]; !ok {
		t.Fatalf("expected matches")
	} else if len(lines) != 2 {
		t.Errorf("want 2 lines (capped), got %v", lines)
	}
}

func TestInprocMultipleFiles(t *testing.T) {
	dir := t.TempDir()
	f1 := writeFile(t, dir, "e1.txt", "needle here\nno match\n")
	f2 := writeFile(t, dir, "e2.txt", "nothing\n")
	f3 := writeFile(t, dir, "e3.txt", "needle there\n")
	got := inproc{}.Matches("needle", []string{f1, f2, f3}, 0)
	if _, ok := got[f1]; !ok {
		t.Error("f1 should match")
	}
	if _, ok := got[f2]; ok {
		t.Error("f2 should not match")
	}
	if _, ok := got[f3]; !ok {
		t.Error("f3 should match")
	}
}

func TestInprocMissingFileSkipped(t *testing.T) {
	got := inproc{}.Matches("foo", []string{"/nonexistent/path/file.txt"}, 0)
	if len(got) != 0 {
		t.Errorf("missing file should be skipped, got %v", got)
	}
}

func TestInprocNoMatchInFile(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "f.txt", "hello world\nbaz qux\n")
	got := inproc{}.Matches("needle", []string{f}, 0)
	if len(got) != 0 {
		t.Errorf("no match should return empty map, got %v", got)
	}
}

func TestInprocLineNumbers1Based(t *testing.T) {
	dir := t.TempDir()
	// match is on first line -> line number 1
	f := writeFile(t, dir, "g.txt", "match\nno\n")
	got := inproc{}.Matches("match", []string{f}, 0)
	if lines := got[f]; len(lines) == 0 || lines[0] != 1 {
		t.Errorf("want line 1 (1-based), got %v", lines)
	}
}

// --- splitMatch tests ---

func TestSplitMatchValid(t *testing.T) {
	file, line, ok := splitMatch("/tmp/session.json:42:some text here")
	if !ok {
		t.Fatal("want ok=true")
	}
	if file != "/tmp/session.json" {
		t.Errorf("file: want /tmp/session.json, got %q", file)
	}
	if line != 42 {
		t.Errorf("line: want 42, got %d", line)
	}
}

func TestSplitMatchNoColon(t *testing.T) {
	_, _, ok := splitMatch("nocolonhere")
	if ok {
		t.Error("want ok=false for no colon")
	}
}

func TestSplitMatchOneColon(t *testing.T) {
	_, _, ok := splitMatch("file.txt:notanumber")
	if ok {
		t.Error("want ok=false for only one colon")
	}
}

func TestSplitMatchNonNumericLine(t *testing.T) {
	_, _, ok := splitMatch("/tmp/file.txt:abc:content")
	if ok {
		t.Error("want ok=false for non-numeric line")
	}
}

func TestSplitMatchEmpty(t *testing.T) {
	_, _, ok := splitMatch("")
	if ok {
		t.Error("want ok=false for empty string")
	}
}

func TestSplitMatchLineOne(t *testing.T) {
	_, line, ok := splitMatch("/abs/path/file.go:1:package main")
	if !ok {
		t.Fatal("want ok=true")
	}
	if line != 1 {
		t.Errorf("want line 1, got %d", line)
	}
}

// --- New() smoke test ---

func TestNewReturnsSearcher(t *testing.T) {
	s := New(config.Config{})
	if s == nil {
		t.Fatal("New should return a non-nil Searcher")
	}
	// Verify the returned searcher actually works.
	dir := t.TempDir()
	f := writeFile(t, dir, "smoke.txt", "hello world\n")
	got := s.Matches("hello", []string{f}, 0)
	if _, ok := got[f]; !ok {
		t.Errorf("New() searcher should find 'hello' in file; got %v", got)
	}
}
