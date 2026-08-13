package main

import (
	"bytes"
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireGitWorkTree skips the guard when the source is not a git work tree
// (e.g. an exported archive on a test box): the guards audit the TRACKED tree,
// which only exists where .git does. CI checkouts always have one, so the
// guards still run everywhere that matters.
func requireGitWorkTree(t *testing.T) {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Skip("requires a git work tree (source was exported without .git); the guard runs in CI checkouts")
	}
}

// repoRoot resolves the repository root so the guards scan the WHOLE tree, not
// just the package directory the test binary runs from. This file lives in
// cmd/ax/, so a bare `git ls-files` would only list cmd/ax/ files and silently
// neuter the guards; every git invocation below sets cmd.Dir = root and reads
// files via filepath.Join(root, p).
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestCoordinatorStaysAtTheFrontDoor: `ax coordinate` made the coordinator a
// first-class verb, but it stays policy at the edge (verb, compose mode, help,
// embed) and never a concept the mechanism layers know about. Fails when the
// word appears in a code token (comments stripped) outside the allowlist.
// The word is assembled from two fragments so this test does not match itself.
func TestCoordinatorStaysAtTheFrontDoor(t *testing.T) {
	requireGitWorkTree(t)
	banned := "coordin" + "ator"

	// The front door: where the concept is allowed to live, and nowhere else.
	frontDoor := map[string]bool{
		"cmd/ax/main.go":                   true, // verb dispatch + usage text
		"internal/app/coordinate.go":       true, // the verb implementation
		"internal/app/coordinate_test.go":  true,
		"internal/app/compose.go":          true, // the picker's compose mode
		"internal/app/compose_test.go":     true,
		"internal/finder/picker.go":        true, // first-run empty-state hint
		"internal/finder/discover_test.go": true,
		"internal/view/help.go":            true, // help screen footer
		"behaviors/embed.go":               true, // the embedded behavior text
	}

	root := repoRoot(t)
	cmd := exec.Command("git", "ls-files", "*.go")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	self := "cmd/ax/guard_test.go"
	var offenders []string
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		p := string(line)
		if p == "" || p == self || strings.HasPrefix(p, "recipes/") || frontDoor[p] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if bannedInCode(data, banned) {
			offenders = append(offenders, p)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("the %s concept must stay at the front door (verb/compose/help/embed), never in the mechanism layers; found the word in a code token in:\n  %s",
			banned, strings.Join(offenders, "\n  "))
	}
}

// bannedInCode reports whether banned appears (case-insensitively) in any
// non-comment token of the Go source src. It uses go/scanner in its default
// mode, which drops comments entirely, so only identifiers, string/char/number
// literals, and other real code tokens are examined. Keyword and operator tokens
// carry no literal text and are skipped. On a scan error the tokenizer keeps
// going; a partial scan is fine for a guard whose only job is to spot the word.
func bannedInCode(src []byte, banned string) bool {
	var s scanner.Scanner
	fset := token.NewFileSet()
	f := fset.AddFile("", fset.Base(), len(src))
	s.Init(f, src, nil, 0) // mode 0: comments are not emitted as tokens
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return false
		}
		if lit != "" && strings.Contains(strings.ToLower(lit), banned) {
			return true
		}
	}
}

// TestBannedInCodeIgnoresComments locks in the narrowed rule: the word is allowed
// in comments but caught in identifiers and string literals.
func TestBannedInCodeIgnoresComments(t *testing.T) {
	banned := "coordin" + "ator"

	cases := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "line comment",
			src:  fmt.Sprintf("package p\n// the %s is a recipe\nvar X = 1\n", banned),
			want: false,
		},
		{
			name: "block comment",
			src:  fmt.Sprintf("package p\n/* about the %s */\nvar X = 1\n", banned),
			want: false,
		},
		{
			name: "identifier",
			src:  fmt.Sprintf("package p\nvar %sState = 1\n", banned),
			want: true,
		},
		{
			name: "string literal",
			src:  fmt.Sprintf("package p\nvar X = %q\n", "the "+banned),
			want: true,
		},
	}

	for _, tc := range cases {
		if got := bannedInCode([]byte(tc.src), banned); got != tc.want {
			t.Errorf("%s: bannedInCode = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestNoInternalArtifactsTracked enforces that agent/LLM internal artifacts
// are never tracked by git. Add new patterns here when a new artifact class needs guarding.
func TestNoInternalArtifactsTracked(t *testing.T) {
	requireGitWorkTree(t)
	cmd := exec.Command("git", "ls-files")
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	// Patterns that must never appear in the tracked tree.
	patterns := []string{
		"CLAUDE.md",
		".claude.md",
		".claude/",
		".coordinator/",
	}

	var offenders []string
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		p := string(line)
		if p == "" {
			continue
		}
		for _, pat := range patterns {
			if matchesInternalPattern(p, pat) {
				offenders = append(offenders, p)
				break
			}
		}
	}

	if len(offenders) > 0 {
		t.Errorf("internal artifacts are tracked by git (run git rm --cached <path> and add to .gitignore):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// matchesInternalPattern returns true if path p matches internal guard pattern pat.
//
// Rules by pattern form:
//   - ends with "/": directory segment match — path must begin with or contain that segment.
//   - starts with ".": suffix match — path must end with that suffix (e.g. ".claude.md").
//   - contains "/": exact path or rooted-suffix match.
//   - otherwise: base-name match anywhere in the tree.
func matchesInternalPattern(p, pat string) bool {
	switch {
	case strings.HasSuffix(pat, "/"):
		return strings.HasPrefix(p, pat) || strings.Contains(p, "/"+pat)
	case strings.HasPrefix(pat, "."):
		// suffix like ".claude.md" matches "foo.claude.md", "dir/foo.claude.md"
		return strings.HasSuffix(p, pat)
	case strings.Contains(pat, "/"):
		return p == pat || strings.HasSuffix(p, "/"+pat)
	default:
		// bare filename: match as base name anywhere
		return p == pat || strings.HasSuffix(p, "/"+pat)
	}
}
