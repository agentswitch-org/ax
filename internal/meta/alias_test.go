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

// A short prefix of a launch id must keep resolving after adoption removes the
// placeholder session (the picker shows shortened ids); an ambiguous prefix
// must resolve to nothing rather than guess.
func TestResolveAliasPrefix(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := SaveAlias("1f6c842c-aaaa-4bbb-8ccc-000000000001", "real-1"); err != nil {
		t.Fatal(err)
	}
	if got, ok := ResolveAliasPrefix("1f6c842c"); !ok || got != "real-1" {
		t.Fatalf("ResolveAliasPrefix(short) = (%q, %v), want (real-1, true)", got, ok)
	}
	if _, ok := ResolveAliasPrefix("ffffffff"); ok {
		t.Fatal("unknown prefix must not resolve")
	}
	if _, ok := ResolveAliasPrefix(""); ok {
		t.Fatal("empty prefix must not resolve")
	}

	if err := SaveAlias("1f6c842c-aaaa-4bbb-8ccc-000000000002", "real-2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := ResolveAliasPrefix("1f6c842c"); ok {
		t.Fatal("ambiguous prefix must not resolve")
	}

	// Aliases exposes the forward map for the list --json reverse join.
	m := Aliases()
	if m["1f6c842c-aaaa-4bbb-8ccc-000000000001"] != "real-1" || m["1f6c842c-aaaa-4bbb-8ccc-000000000002"] != "real-2" {
		t.Fatalf("Aliases = %v, want both forward pointers", m)
	}
}
