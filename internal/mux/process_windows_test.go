//go:build windows

package mux

import "testing"

// TestBackendSelectionWindows pins the Windows selection contract: with no mux
// configured (or the explicit "process"), ax runs the winproc backend, since
// no terminal multiplexer exists to shell out to; an explicit real mux is
// still honored.
func TestBackendSelectionWindows(t *testing.T) {
	if _, ok := backend("").(winproc); !ok {
		t.Errorf("backend(\"\") = %T, want winproc", backend(""))
	}
	if _, ok := backend("process").(winproc); !ok {
		t.Errorf("backend(\"process\") = %T, want winproc", backend("process"))
	}
	if _, ok := backend("tmux").(tmux); !ok {
		t.Errorf("backend(\"tmux\") = %T, want tmux", backend("tmux"))
	}
	if _, ok := backend("zellij").(zellij); !ok {
		t.Errorf("backend(\"zellij\") = %T, want zellij", backend("zellij"))
	}
	if _, ok := backend("none").(none); !ok {
		t.Errorf("backend(\"none\") = %T, want none", backend("none"))
	}

	for name, want := range map[string]bool{
		"": true, "process": true, "tmux": false, "zellij": false, "none": false,
	} {
		if got := isProcessMux(name); got != want {
			t.Errorf("isProcessMux(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestWinprocWindowOps pins the honest no-op/error contract for the methods
// that only mean something against a terminal window.
func TestWinprocWindowOps(t *testing.T) {
	w := winproc{}
	if !w.Active() {
		t.Error("Active() = false, want true (the launcher must take the Open path)")
	}
	if w.HasWindows() {
		t.Error("HasWindows must be false for the Windows process backend")
	}
	if _, ok := w.Locate("x"); ok {
		t.Error("Locate found a window for a windowless backend")
	}
	if w.Live() != nil || w.Panes() != nil || w.PaneTail("x", 5) != "" {
		t.Error("Live/Panes/PaneTail must be empty for a windowless backend")
	}
	if err := w.MoveWindow("x", "t"); err == nil {
		t.Error("MoveWindow must error (not silently no-op)")
	}
	if err := w.CloseWindow("x"); err == nil {
		t.Error("CloseWindow must error (not silently no-op)")
	}
	if err := w.Send("", "hi", true); err == nil {
		t.Error("Send with no session id must error")
	}
}
