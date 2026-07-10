//go:build unix

package mux

import (
	"path/filepath"
	"testing"
)

// backend maps the config value to the right implementation, defaulting to tmux
// for empty or unknown so an unset config keeps today's behavior. (The Windows
// mapping, where the process backend is the default, is pinned in
// process_windows_test.go.)
func TestBackendSelector(t *testing.T) {
	cases := map[string]any{
		"tmux":    tmux{},
		"zellij":  zellij{},
		"process": process{},
		"none":    none{},
		"":        tmux{},
		"unknown": tmux{},
	}
	for name, want := range cases {
		got := backend(name)
		if wantType, gotType := typeName(want), typeName(got); wantType != gotType {
			t.Errorf("backend(%q) = %s, want %s", name, gotType, wantType)
		}
	}
}

// With no `mux` set (here: no config file at all), New defaults to tmux.
func TestNewDefaultsToTmux(t *testing.T) {
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	if _, ok := New().(tmux); !ok {
		t.Fatalf("New() = %T, want tmux", New())
	}
}

func typeName(v any) string {
	switch v.(type) {
	case tmux:
		return "tmux"
	case zellij:
		return "zellij"
	case process:
		return "process"
	case none:
		return "none"
	default:
		return "?"
	}
}
