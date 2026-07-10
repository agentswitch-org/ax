package finder

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

// loadingPicker is a picker in the state it starts in when opened with
// View.Load: no sessions yet, a zero Config, loading=true. This is the state
// the very first frame renders from, before the background load lands.
func loadingPicker() *picker {
	return &picker{
		meta:         map[string]view.RowMeta{},
		collapsed:    map[string]bool{},
		marks:        map[string]bool{},
		visual:       -1,
		km:           keys.Build(nil),
		loading:      true,
		previewCache: map[string][]string{}, // run() sets this up before load kicks off
		fetching:     map[string]bool{},
	}
}

// The loading skeleton must render safely with no sessions and a zero Config
// (column layout falls back to defaults), and say so in the frame, so the
// picker's first paint never waits on the background load.
func TestLoadingFrameRendersSafely(t *testing.T) {
	p := loadingPicker()
	p.recompute()
	p.sc = &screen{cols: 80, rows: 24}
	lines := p.frameLines()
	if len(lines) == 0 {
		t.Fatalf("loading skeleton produced no frame")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(view.StripANSI(joined), "loading") {
		t.Fatalf("loading skeleton should say it's loading, got:\n%s", joined)
	}
}

func TestEmptyFrameShowsNoticeInListArea(t *testing.T) {
	p := loadingPicker()
	p.loading = false
	p.notice = "scope: working hid all 98 sessions - press t to change scope"
	p.recompute()
	p.sc = &screen{cols: 80, rows: 24}

	joined := view.StripANSI(strings.Join(p.frameLines(), "\n"))
	if !strings.Contains(joined, p.notice) {
		t.Fatalf("empty frame should show notice in the list area, got:\n%s", joined)
	}
}

// While loading, actions that assume the real config/sessions are in (new
// session, column cycling, kill, open) must no-op rather than act on a
// half-initialized picker; only Quit stays live, so the skeleton is always
// cancelable.
func TestLoadingBlocksActionsExceptQuit(t *testing.T) {
	p := loadingPicker()
	p.recompute()

	if done := p.dispatch(keys.New); done {
		t.Fatalf("New should not end the picker while loading")
	}
	if p.choice.New {
		t.Fatalf("New should not set choice.New while loading")
	}
	if done := p.dispatch(keys.NewArgs); done {
		t.Fatalf("NewArgs should not end the picker while loading")
	}
	if !p.dispatch(keys.Quit) {
		t.Fatalf("Quit should still end the picker while loading")
	}
}

// applyInitialLoad replaces the loading skeleton with the real sessions and
// clears the loading flag, exactly as the background load's result does when
// it lands on run()'s loadReady channel.
func TestApplyInitialLoadPopulatesAndClearsLoading(t *testing.T) {
	p := loadingPicker()
	p.recompute()

	sessions := []session.Session{{ID: "a", Title: "alpha"}}
	p.applyInitialLoad(View{Sessions: sessions, Meta: map[string]view.RowMeta{"a": {}}})

	if p.loading {
		t.Fatalf("loading should be false after applyInitialLoad")
	}
	if len(p.matches) != 1 {
		t.Fatalf("expected 1 match after load, got %d", len(p.matches))
	}
	if s, ok := p.cur(); !ok || s.ID != "a" {
		t.Fatalf("cursor should land on the loaded session, got %v", s)
	}
}
