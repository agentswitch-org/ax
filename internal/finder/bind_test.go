package finder

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/session"
)

// expandBind must fill every documented placeholder from the selected session,
// plus the binding's own fixed {file}, so a bound command sees exactly what the
// config promises.
func TestExpandBindFillsSessionAndFilePlaceholders(t *testing.T) {
	s := session.Session{ID: "abc123", Group: "myrun", Dir: "/home/n/proj", File: "/home/n/.claude/x.jsonl"}
	got := expandBind("open {id} {run} {dir} {transcript} {file}", s, "~/backlog.md")
	for _, want := range []string{"'abc123'", "'myrun'", "'/home/n/proj'", "'/home/n/.claude/x.jsonl'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expandBind(...) = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "~/backlog.md") {
		t.Fatalf("expandBind(...) = %q, {file} should have been ~-expanded", got)
	}
}

// A quote embedded in an untrusted value (a session id or dir path someone
// engineered) must not let it break out of the single-quoted substitution.
func TestExpandBindEscapesQuotesInValues(t *testing.T) {
	s := session.Session{ID: "a'; rm -rf /", Dir: "/tmp"}
	got := expandBind("echo {id}", s, "")
	if !strings.Contains(got, `'a'\''; rm -rf /'`) {
		t.Fatalf("expandBind(...) = %q, want a properly escaped single-quoted id", got)
	}
}

// Placeholders for a value the session (or the binding) does not have expand
// to an empty quoted string, not a literal "{run}" left in the command.
func TestExpandBindEmptyValuesExpandToEmptyQuotes(t *testing.T) {
	got := expandBind("ax tag {id} --run {run}", session.Session{ID: "solo"}, "")
	if strings.Contains(got, "{run}") {
		t.Fatalf("expandBind(...) = %q, left a placeholder unexpanded", got)
	}
	if !strings.Contains(got, "--run ''") {
		t.Fatalf("expandBind(...) = %q, want an empty run to expand to ''", got)
	}
}

// A [[bind]] key normalizes through keys.NormKey the same way the built-in
// keymap does, so "space" in config matches the actual space keypress.
func TestFindBindMatchesNormalizedKey(t *testing.T) {
	binds := []config.Bind{{Key: "space", Run: "true"}}
	if _, ok := findBind(binds, " "); !ok {
		t.Fatal("a \"space\" bind should match a literal space keypress")
	}
	if _, ok := findBind(binds, "space"); ok {
		t.Fatal("a \"space\" bind should not match the literal string \"space\"")
	}
}

// A key with no matching bind resolves to nothing, so an unmapped chord after
// the leader is a no-op instead of running the wrong command.
func TestFindBindNoMatch(t *testing.T) {
	binds := []config.Bind{{Key: "e", Run: "true"}}
	if _, ok := findBind(binds, "z"); ok {
		t.Fatal("no bind is configured for \"z\"")
	}
}
