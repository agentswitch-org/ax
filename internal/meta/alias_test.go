package meta

import "testing"

// TestAliasRoundTrip pins the launch-id contract for mint-its-own-id
// harnesses: the id a launch printed resolves to the adopted real session id,
// an unknown id resolves to itself, and degenerate saves are no-ops.
func TestAliasRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := SaveAlias("launch-1", "real-1"); err != nil {
		t.Fatal(err)
	}
	if got := ResolveAlias("launch-1"); got != "real-1" {
		t.Fatalf("ResolveAlias = %q, want real-1", got)
	}
	if got := ResolveAlias("no-alias"); got != "no-alias" {
		t.Fatalf("unknown id must resolve to itself, got %q", got)
	}
	if got := ResolveAlias(""); got != "" {
		t.Fatalf("empty id must resolve to empty, got %q", got)
	}

	// Self- and empty-target aliases are never written.
	if err := SaveAlias("x", "x"); err != nil {
		t.Fatal(err)
	}
	if err := SaveAlias("y", ""); err != nil {
		t.Fatal(err)
	}
	if got := ResolveAlias("y"); got != "y" {
		t.Fatalf("empty-target alias must not resolve, got %q", got)
	}

	RemoveAlias("launch-1")
	if got := ResolveAlias("launch-1"); got != "launch-1" {
		t.Fatalf("removed alias must resolve to itself, got %q", got)
	}
}
