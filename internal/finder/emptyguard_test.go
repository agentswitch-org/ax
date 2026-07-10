package finder

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/retention"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

// After a mass crash every session resolves to crash/inactive: scopeLive would
// otherwise filter the whole list to empty with no explanation (while `ax
// list` still shows everything). recompute must fall back to the unscoped set
// and leave a notice saying why.
func TestScopeLiveEmptyGuardFallsBackWithNotice(t *testing.T) {
	sessions := []session.Session{
		{ID: "a"},
		{ID: "b"},
		{ID: "c"},
	}
	meta := map[string]view.RowMeta{
		"a": {}, // inactive: no live state
		"b": {},
		"c": {},
	}
	p := newTestPicker(sessions, meta)
	p.scope = scopeLive
	p.recompute()

	if len(p.matches) != len(sessions) {
		t.Fatalf("scopeLive with all rows inactive should fall back to the full set, got %d matches", len(p.matches))
	}
	if p.notice == "" || !strings.Contains(p.notice, "3") || !strings.Contains(p.notice, "live") {
		t.Fatalf("expected a notice explaining the scope hid all 3 sessions, got %q", p.notice)
	}
	// The persisted scope intent is untouched: it's still Live afterwards.
	if p.scope != scopeLive {
		t.Fatalf("scope should remain Live after the fallback, got %v", p.scope)
	}
}

// When some sessions are actually live, scopeLive must keep filtering
// normally: no fallback, no notice.
func TestScopeLiveNoFallbackWhenSomeSessionsLive(t *testing.T) {
	sessions := []session.Session{
		{ID: "work"},
		{ID: "dead"},
	}
	meta := map[string]view.RowMeta{
		"work": {State: view.StateLive, Activity: view.Working},
		"dead": {},
	}
	p := newTestPicker(sessions, meta)
	p.scope = scopeLive
	p.recompute()

	if len(p.matches) != 1 {
		t.Fatalf("scopeLive should keep only the live session, got %d matches", len(p.matches))
	}
	if s, ok := p.rowSession(p.matches[0]); !ok || s.ID != "work" {
		t.Fatalf("expected the surviving row to be 'work', got %v", p.matches)
	}
	if p.notice != "" {
		t.Fatalf("no fallback notice expected when scope still shows rows, got %q", p.notice)
	}
}

// Archived-only history should not look erased when the archive filter is on
// its default unarchived view. Show the hidden rows for this render and tell the
// user which key reveals the archive tiers.
func TestArchiveFilterEmptyGuardFallsBackWithNotice(t *testing.T) {
	sessions := []session.Session{
		{ID: "old-a", Archived: true},
		{ID: "old-b", Archived: true},
	}
	p := newTestPicker(sessions, map[string]view.RowMeta{})
	p.archive = retention.ActiveOnly
	p.recompute()

	if len(p.matches) != len(sessions) {
		t.Fatalf("archive filter should fall back to the archived rows, got %d matches", len(p.matches))
	}
	if p.notice == "" || !strings.Contains(p.notice, "archive") || !strings.Contains(p.notice, "A") {
		t.Fatalf("expected archive notice with key hint, got %q", p.notice)
	}
	if p.archive != retention.ActiveOnly {
		t.Fatalf("archive preference should remain active-only after fallback, got %v", p.archive)
	}
}

// If scope and archive filters jointly hide every row, reveal the full set for
// this render and name both controls so the view is resettable without editing
// ui.json.
func TestCombinedScopeAndArchiveEmptyGuardFallsBackWithNotice(t *testing.T) {
	sessions := []session.Session{
		{ID: "archived-dead", Archived: true},
	}
	p := newTestPicker(sessions, map[string]view.RowMeta{"archived-dead": {}})
	p.scope = scopeLive
	p.archive = retention.ActiveOnly
	p.recompute()

	if len(p.matches) != len(sessions) {
		t.Fatalf("combined filters should fall back to all rows, got %d matches", len(p.matches))
	}
	if p.notice == "" || !strings.Contains(p.notice, "scope") || !strings.Contains(p.notice, "archive") {
		t.Fatalf("expected combined filter notice, got %q", p.notice)
	}
	if p.scope != scopeLive || p.archive != retention.ActiveOnly {
		t.Fatalf("preferences should remain unchanged after fallback: scope=%v archive=%v", p.scope, p.archive)
	}
}
