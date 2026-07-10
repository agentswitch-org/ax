package keys

import "testing"

// The run filter's config key is "runs" (formerly "groups"); a config still
// using the deprecated "groups" key must keep rebinding it.
func TestBuildAcceptsDeprecatedGroupsKeyAlias(t *testing.T) {
	m := Build(map[string][]string{"groups": {"z"}})
	if got := m.Key(Groups); got != "z" {
		t.Fatalf("Build with deprecated %q override: Key(Groups) = %q, want %q", "groups", got, "z")
	}
	if a := m.Lookup("z"); a != Groups {
		t.Fatalf("Lookup(%q) = %q, want Groups", "z", a)
	}
}

// The current "runs" key takes precedence over the deprecated "groups" alias
// when both are set.
func TestBuildPrefersRunsOverDeprecatedGroupsKey(t *testing.T) {
	m := Build(map[string][]string{"groups": {"z"}, "runs": {"y"}})
	if got := m.Key(Groups); got != "y" {
		t.Fatalf("Key(Groups) = %q, want %q (current key wins)", got, "y")
	}
}

// c, C, AND ctrl-n all resolve to the single Compose entry by default, and the
// old quick-new actions (New/NewArgs) carry no default key: the compose flow's
// first choice is plain-new, so c/C/ctrl-n are the one "launch an ax-managed
// harness" verb. ctrl-n matters because it is the only Compose key that fires in
// filter/insert mode (where plain runes type into the query), preserving the old
// ctrl-n muscle memory.
func TestComposeIsDefaultForCandCUpper(t *testing.T) {
	m := Build(nil)
	if a := m.Lookup("c"); a != Compose {
		t.Fatalf(`Lookup("c") = %q, want Compose`, a)
	}
	if a := m.Lookup("C"); a != Compose {
		t.Fatalf(`Lookup("C") = %q, want Compose`, a)
	}
	if a := m.Lookup("ctrl-n"); a != Compose {
		t.Fatalf(`Lookup("ctrl-n") = %q, want Compose (filter-mode launch shortcut)`, a)
	}
	if k := m.Key(New); k != "" {
		t.Fatalf("New default key = %q, want none", k)
	}
	if k := m.Key(NewArgs); k != "" {
		t.Fatalf("NewArgs default key = %q, want none", k)
	}
}

// An explicit `[keys] new = "c"` override still binds quick-new to c (not
// compose): a user who intentionally rebinds new must keep it, so the compose
// default never silently reinterprets their config.
func TestExplicitNewOverrideWinsOverComposeDefault(t *testing.T) {
	m := Build(map[string][]string{"new": {"c"}})
	if a := m.Lookup("c"); a != New {
		t.Fatalf(`Lookup("c") with new=c override = %q, want New`, a)
	}
	if a := m.Lookup("C"); a != Compose {
		t.Fatalf(`Lookup("C") = %q, want Compose (still the default)`, a)
	}
	if a := m.Lookup("ctrl-n"); a != Compose {
		t.Fatalf(`Lookup("ctrl-n") = %q, want Compose (unaffected by the new=c override)`, a)
	}
}

// The preview navigation keys must not shadow the session-list go-to-top /
// go-to-bottom row shortcuts: g still jumps the row cursor to the top and G to
// the bottom, while the preview scroll/jump keys resolve to their own actions.
func TestPreviewKeysDoNotShadowListTopBottom(t *testing.T) {
	m := Build(nil)
	if a := m.Lookup("g"); a != Top {
		t.Fatalf(`Lookup("g") = %q, want Top (list go-to-top must stay intact)`, a)
	}
	if a := m.Lookup("G"); a != Bottom {
		t.Fatalf(`Lookup("G") = %q, want Bottom (list go-to-bottom must stay intact)`, a)
	}
	for key, want := range map[string]Action{
		"J":      PreviewDown,
		"K":      PreviewUp,
		"ctrl-d": PreviewHalfDown,
		"ctrl-u": PreviewHalfUp,
		"ctrl-g": PreviewTop,
		"ctrl-e": PreviewBottom,
	} {
		got := m.Lookup(key)
		if got != want {
			t.Fatalf("Lookup(%q) = %q, want %q", key, got, want)
		}
		if got == Top || got == Bottom {
			t.Fatalf("preview key %q resolves to a list row action %q", key, got)
		}
	}
	// The list half-page keys (plain d/u) are distinct from the preview
	// half-page keys (ctrl-d/ctrl-u).
	if a := m.Lookup("d"); a != HalfDown {
		t.Fatalf(`Lookup("d") = %q, want HalfDown (list half-page)`, a)
	}
	if a := m.Lookup("u"); a != HalfUp {
		t.Fatalf(`Lookup("u") = %q, want HalfUp (list half-page)`, a)
	}
}

func TestToggleArchivedDefaultKey(t *testing.T) {
	m := Build(nil)
	if a := m.Lookup("D"); a != ToggleArchived {
		t.Fatalf(`Lookup("D") = %q, want ToggleArchived`, a)
	}
	if k := m.Key(ToggleArchived); k != "D" {
		t.Fatalf("Key(ToggleArchived) = %q, want D", k)
	}
}
