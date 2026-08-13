package finder

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

// Both hint rows carry every daily action with an unabbreviated label, and the
// primary actions (resume, new) lead the first row.
func TestHintRowsAreCompleteAndUnabbreviated(t *testing.T) {
	p := loadingPicker()
	p.loading = false
	row1 := view.StripANSI(p.hintLine())
	row2 := view.StripANSI(p.hintLine2())

	for _, want := range []string{"enter resume", "c new session", "x kill", "D archive", "l tag", "w close window", "o open window", "detach"} {
		if !strings.Contains(row1, want) {
			t.Errorf("hint row 1 %q missing %q", row1, want)
		}
	}
	if !strings.HasPrefix(row1, "enter resume") {
		t.Errorf("hint row 1 must lead with resume, got %q", row1)
	}
	for _, want := range []string{"move", "/ search text", "i filter rows", "' quick-switch", "v multi-select", "t scope", "A show archived", "b group", "? all keys"} {
		if !strings.Contains(row2, want) {
			t.Errorf("hint row 2 %q missing %q", row2, want)
		}
	}

	// While typing, the second row explains the mode instead.
	p.mode = mFilter
	if got := view.StripANSI(p.hintLine2()); !strings.Contains(got, "esc clears") {
		t.Errorf("filter-mode hint = %q", got)
	}
	p.mode = mContent
	if got := view.StripANSI(p.hintLine2()); !strings.Contains(got, "searches every transcript") {
		t.Errorf("search-mode hint = %q", got)
	}
}

// With no sessions at all the list area teaches how to start, instead of
// rendering blank rows (the first-run experience).
func TestEmptyStateTeachesHowToStart(t *testing.T) {
	p := loadingPicker()
	p.loading = false
	p.recompute()
	p.sc = &screen{cols: 120, rows: 30}
	frame := view.StripANSI(strings.Join(p.frameLines(), "\n"))
	for _, want := range []string{"no sessions yet", "start a session", "ax coordinate"} {
		if !strings.Contains(frame, want) {
			t.Errorf("empty-state frame missing %q", want)
		}
	}

	// The hero never renders once sessions exist, or while loading.
	p.all = []session.Session{{ID: "x"}}
	if lines := p.emptyStateLines(); lines != nil {
		t.Errorf("emptyStateLines with sessions = %d lines, want none", len(lines))
	}
	p.all = nil
	p.loading = true
	if lines := p.emptyStateLines(); lines != nil {
		t.Error("emptyStateLines while loading, want none")
	}
}
