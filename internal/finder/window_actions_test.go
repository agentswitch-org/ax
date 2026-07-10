package finder

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

// fakeMux is a minimal Multiplexer for the picker's window actions: Locate
// reports whether a session's window is currently "open" (keyed by session.Key),
// which is all windowsOpen reads. Active is false so refreshMeta short-circuits
// its adopt work; every other method is an inert no-op.
type fakeMux struct{ open map[string]bool }

func (f *fakeMux) Active() bool                            { return false }
func (f *fakeMux) HasWindows() bool                        { return true }
func (f *fakeMux) Open(_, _, _, _, _ string, _ bool) error { return nil }
func (f *fakeMux) Locate(id string) (string, bool) {
	if f.open[id] {
		return "@" + id, true
	}
	return "", false
}
func (f *fakeMux) Live() map[string]string         { return nil }
func (f *fakeMux) Panes() []mux.Pane               { return nil }
func (f *fakeMux) Focus(string) error              { return nil }
func (f *fakeMux) Send(string, string, bool) error { return nil }
func (f *fakeMux) Interrupt(string) error          { return nil }
func (f *fakeMux) PaneTail(string, int) string     { return "" }
func (f *fakeMux) MoveWindow(string, string) error { return nil }
func (f *fakeMux) CloseWindow(string) error        { return nil }
func (f *fakeMux) Retag(string) error              { return nil }

func windowActionPicker(mx mux.Multiplexer) *picker {
	p := inputPicker()
	p.mx = mx
	p.meta = map[string]view.RowMeta{}
	p.metaReady = make(chan map[string]view.RowMeta, 1) // absorb refreshMeta's background send
	p.recompute()
	return p
}

// detachWindows resolves the acted-on set exactly like the other multi-select
// actions (visual range or marks, falling back to the cursor row) and hands it to
// the injected callback. The footer status counts the real window-presence delta,
// so a session the callback skips (e.g. unheld) is reported as left up, not
// detached.
func TestDetachWindowsSelectionAndSkip(t *testing.T) {
	mx := &fakeMux{open: map[string]bool{"a": true, "b": true, "c": true}}
	p := windowActionPicker(mx)

	var got []session.Session
	// Simulate the app: close every selected window except "b" (stand-in for an
	// unheld session the guard skips), by dropping it from the open set.
	p.onDetachWindows = func(sel []session.Session) {
		got = sel
		for _, s := range sel {
			if s.ID != "b" {
				delete(mx.open, session.Key(s))
			}
		}
	}

	p.dispatch(keys.Visual) // anchor on "a"
	p.move(1)               // extend to "b"
	p.dispatch(keys.DetachWin)

	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("detach acted on %v, want the a+b visual range", ids(got))
	}
	notice := view.StripANSI(p.notice)
	if !strings.Contains(notice, "detached 1 window") {
		t.Errorf("notice = %q, want it to report 1 detached", notice)
	}
	if !strings.Contains(notice, "left 1 up") {
		t.Errorf("notice = %q, want it to report the 1 skipped (unheld) window", notice)
	}
	// selection is cleared after the action, vim-style.
	if len(p.marks) != 0 || p.visual != -1 {
		t.Errorf("selection not cleared: marks=%v visual=%d", p.marks, p.visual)
	}
}

// With no marks and no visual range, both window actions fall back to the cursor
// row, the same single-row fallback selection() gives every other action.
func TestWindowActionsCursorFallback(t *testing.T) {
	mx := &fakeMux{open: map[string]bool{}}
	p := windowActionPicker(mx)
	p.cursor = 2 // the "c" row

	var detachGot, openGot []session.Session
	p.onDetachWindows = func(sel []session.Session) { detachGot = sel }
	p.onOpenWindows = func(sel []session.Session) {
		openGot = sel
		for _, s := range sel { // simulate the app bringing each window up
			mx.open[session.Key(s)] = true
		}
	}

	p.dispatch(keys.DetachWin)
	if len(detachGot) != 1 || detachGot[0].ID != "c" {
		t.Fatalf("detach fallback acted on %v, want just the cursor row c", ids(detachGot))
	}

	p.dispatch(keys.OpenWin)
	if len(openGot) != 1 || openGot[0].ID != "c" {
		t.Fatalf("open fallback acted on %v, want just the cursor row c", ids(openGot))
	}
	if notice := view.StripANSI(p.notice); !strings.Contains(notice, "opened 1 window") {
		t.Errorf("open notice = %q, want it to report 1 opened", notice)
	}
}

// reopenWindows acts on the marked set (not just the cursor) and reports how many
// windows came up by the presence delta.
func TestOpenWindowsMarkedSet(t *testing.T) {
	mx := &fakeMux{open: map[string]bool{}}
	p := windowActionPicker(mx)
	p.marks["a"] = true
	p.marks["c"] = true

	var got []session.Session
	p.onOpenWindows = func(sel []session.Session) {
		got = sel
		for _, s := range sel {
			mx.open[session.Key(s)] = true
		}
	}
	p.dispatch(keys.OpenWin)

	if len(got) != 2 {
		t.Fatalf("open acted on %v, want the 2 marked sessions", ids(got))
	}
	if notice := view.StripANSI(p.notice); !strings.Contains(notice, "opened 2 window") {
		t.Errorf("open notice = %q, want 2 opened", notice)
	}
}

// A nil callback is inert: pressing the key without an injected handler must not
// panic (a caller that did not wire the app-side action).
func TestWindowActionsNilCallback(t *testing.T) {
	p := windowActionPicker(&fakeMux{open: map[string]bool{}})
	p.onDetachWindows = nil
	p.onOpenWindows = nil
	p.dispatch(keys.DetachWin)
	p.dispatch(keys.OpenWin)
}

func ids(ss []session.Session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}
