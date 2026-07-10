package finder

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

func inputPicker() *picker {
	return &picker{
		cfg: config.Config{Columns: []string{"dir", "title"}},
		all: []session.Session{
			{ID: "a", Dir: "/src/blog/app", Title: "blog one"},
			{ID: "b", Dir: "/src/blog/app", Title: "blog two"},
			{ID: "c", Dir: "/src/ax", Title: "other"},
		},
		meta:      map[string]view.RowMeta{},
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		visual:    -1,
		km:        keys.Build(nil),
		selCol:    1, // TITLE
	}
}

func TestEnterCommitsFilter(t *testing.T) {
	p := inputPicker()
	p.enter(mFilter)
	for _, r := range "blog" {
		p.handleInsert(ev{t: evRune, r: r})
	}
	if len(p.matches) != 2 {
		t.Fatalf("filtered matches = %v", p.matches)
	}
	// Enter closes the input but keeps the filtered set; it must NOT open
	if done := p.handleInsert(ev{t: evEnter}); done {
		t.Fatal("Enter in filter mode ended the picker (opened a session)")
	}
	if p.mode != mNormal || p.committed != mFilter {
		t.Fatalf("mode=%d committed=%d", p.mode, p.committed)
	}
	if len(p.matches) != 2 {
		t.Fatalf("committed filter lost: matches = %v", p.matches)
	}
	if seg := view.StripANSI(p.statusSegment()); !strings.Contains(seg, "filter:TITLE:blog") {
		t.Fatalf("header missing committed filter: %q", seg)
	}
	// Esc in normal mode clears the committed filter
	p.handleNormal(ev{t: evEsc})
	if p.committed != 0 || len(p.matches) != 3 {
		t.Fatalf("Esc should clear the filter: committed=%d matches=%v", p.committed, p.matches)
	}
}

func TestVisualSelectTag(t *testing.T) {
	p := inputPicker()
	p.recompute()
	p.dispatch(keys.Visual) // anchor at row 0
	p.move(1)               // extend to row 1
	if got := p.visualSessions(); len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("visual range = %v", got)
	}
	// selection() includes the live range (v-jj-l without a second v)
	if sel := p.selection(); len(sel) != 2 {
		t.Fatalf("selection = %d rows", len(sel))
	}
	// Enter in visual marks the range instead of opening
	if done := p.open(); done {
		t.Fatal("Enter in visual opened sessions")
	}
	if p.visual != -1 || !p.marks["a"] || !p.marks["b"] || p.marks["c"] {
		t.Fatalf("visual commit wrong: visual=%d marks=%v", p.visual, p.marks)
	}
	// Esc cancels an active visual without marking
	p.marks = map[string]bool{}
	p.dispatch(keys.Visual)
	p.move(1)
	p.handleNormal(ev{t: evEsc})
	if p.visual != -1 || len(p.marks) != 0 {
		t.Fatalf("Esc should cancel visual cleanly: visual=%d marks=%v", p.visual, p.marks)
	}
}

func TestVisualSkipsHeaders(t *testing.T) {
	p := inputPicker()
	p.groupBy = "dir"
	p.recompute() // header, a, b, header, c
	p.cursor = 1  // anchor on a session row
	p.dispatch(keys.Visual)
	p.cursor = len(p.matches) - 1
	got := p.visualSessions()
	if len(got) != 3 {
		t.Fatalf("range over headers should yield 3 sessions, got %d", len(got))
	}
}

func TestHeaderSelectsGroup(t *testing.T) {
	p := inputPicker()
	p.groupBy = "dir"
	p.recompute()
	p.cursor = 0 // the /src/blog/app header (2 members)
	p.dispatch(keys.Visual)
	if len(p.marks) != 2 || !p.marks["a"] || !p.marks["b"] {
		t.Fatalf("v on a header should select its group: %v", p.marks)
	}
	// works collapsed too (members ride the header), and toggles off
	p.headerToggle()
	p.cursor = 0
	p.dispatch(keys.Visual)
	if len(p.marks) != 0 {
		t.Fatalf("v again should deselect the group: %v", p.marks)
	}
}

func TestVisualSurvivesResort(t *testing.T) {
	p := inputPicker()
	p.recompute()
	p.cursor = 0
	p.dispatch(keys.Visual) // anchor on session "a"
	p.cursor = 1            // range a..b
	// the list re-sorts underneath the selection (a moves to the end)
	p.all[0], p.all[1], p.all[2] = p.all[2], p.all[0], p.all[1]
	p.recompute()
	if p.visual != 1 { // "a" re-anchored at its new row
		t.Fatalf("anchor did not follow the session: visual=%d", p.visual)
	}
}
