package finder

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

// labelFilter builds a picker with sessions whose TAGS cell would be truncated,
// then returns the match IDs for the given key=value query.
func labelFilter(t *testing.T, query string, sessions []session.Session) []string {
	t.Helper()
	// TAGS column width is 18; a label like "project=agentswitch" (20 chars)
	// would be clipped in the rendered cell but must still match.
	p := &picker{
		cfg:       config.Config{Columns: []string{"tags", "title"}},
		all:       sessions,
		meta:      map[string]view.RowMeta{},
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		visual:    -1,
		km:        keys.Build(nil),
	}
	p.recompute()
	p.query = query
	p.mode = mFilter
	p.filterCol = 0
	p.filterKey = view.ColumnKey(p.cfg, 0)
	p.recompute()
	var ids []string
	for mi := range p.matches { // range by index, not value; rowSession expects a match-list index
		if s, ok := p.rowSession(mi); ok {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

func filterMatchIDs(p *picker) []string {
	var ids []string
	for mi := range p.matches {
		if s, ok := p.rowSession(mi); ok {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

func typeFilter(p *picker, q string) {
	p.enter(mFilter)
	for _, r := range q {
		p.handleInsert(ev{t: evRune, r: r})
	}
}

func TestInsertFilterUsesSelectedNameColumn(t *testing.T) {
	p := &picker{
		cfg: config.Config{Columns: []string{"status", "name", "run", "title"}},
		all: []session.Session{
			{ID: "relay-5", Name: "Relay 5", Group: "alpha", Title: "status report"},
			{ID: "relay-6", Name: "Relay 6", Group: "beta", Title: "handoff note"},
			{ID: "title-only", Name: "Archive", Group: "gamma", Title: "Relay mentioned here"},
		},
		meta:      map[string]view.RowMeta{},
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		visual:    -1,
		km:        keys.Build(nil),
		selCol:    1, // NAME
	}
	p.recompute()
	typeFilter(p, "Relay")

	got := filterMatchIDs(p)
	if len(got) != 2 || got[0] != "relay-5" || got[1] != "relay-6" {
		t.Fatalf("NAME filter returned %v, want only Relay-named sessions", got)
	}
}

func TestInsertFilterUsesSelectedNonNameColumn(t *testing.T) {
	p := &picker{
		cfg: config.Config{Columns: []string{"name", "run", "title"}},
		all: []session.Session{
			{ID: "run-hit", Name: "Damar", Group: "gamma", Title: "ordinary"},
			{ID: "name-only", Name: "Gamma Ray", Group: "alpha", Title: "ordinary"},
			{ID: "title-only", Name: "Kira", Group: "beta", Title: "gamma analysis"},
		},
		meta:      map[string]view.RowMeta{},
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		visual:    -1,
		km:        keys.Build(nil),
		selCol:    1, // RUN
	}
	p.recompute()
	typeFilter(p, "gamma")

	got := filterMatchIDs(p)
	if len(got) != 1 || got[0] != "run-hit" {
		t.Fatalf("RUN filter returned %v, want only the gamma run", got)
	}
}

func TestInsertFilterCapturesSelectedColumn(t *testing.T) {
	p := &picker{
		cfg: config.Config{Columns: []string{"name", "run", "title"}},
		all: []session.Session{
			{ID: "name-hit", Name: "Relay", Group: "alpha", Title: "ordinary"},
			{ID: "run-hit", Name: "Archive", Group: "Relay", Title: "ordinary"},
		},
		meta:      map[string]view.RowMeta{},
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		visual:    -1,
		km:        keys.Build(nil),
		selCol:    0, // NAME
	}
	p.recompute()
	typeFilter(p, "Relay")
	p.handleInsert(ev{t: evEnter})
	p.dispatch(keys.ColNext) // selected header moves to RUN after commit
	p.recompute()

	got := filterMatchIDs(p)
	if len(got) != 1 || got[0] != "name-hit" {
		t.Fatalf("committed filter retargeted after H/L: got %v", got)
	}
}

// Label filter must return 100% of sessions carrying a label even when the
// TAGS cell clips the label mid-value. A label "project=agentswitch" is 20
// chars and gets clipped in the 18-wide TAGS column; filtering must use the full
// column value, not the clipped render text.
func TestLabelFilterReturnsAll(t *testing.T) {
	sessions := []session.Session{
		// label fits in 18-char cell
		{ID: "short", Title: "s", Labels: []string{"project=ax"}},
		// "project=agentswitch" is 20 chars, overflows the 18-wide TAGS cell
		{ID: "long", Title: "l", Labels: []string{"project=agentswitch"}},
		// has the label plus a second one that pushes it even further out
		{ID: "extra", Title: "e", Labels: []string{"env=prod", "project=agentswitch"}},
		// different project: must NOT appear
		{ID: "other", Title: "o", Labels: []string{"project=blogwork"}},
		// no labels: must NOT appear
		{ID: "none", Title: "n"},
	}

	got := labelFilter(t, "project=agentswitch", sessions)
	want := map[string]bool{"long": true, "extra": true}
	if len(got) != 2 {
		t.Fatalf("label filter returned %v, want exactly long and extra", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected session %q in label filter results", id)
		}
	}
}

// A committed key=value filter must be an exact subset in the current sort
// order: no fuzzy-subsequence leakage, no rank order clobbering the sort.
func TestCommittedFilterExactAndSorted(t *testing.T) {
	p := &picker{
		cfg: config.Config{Columns: []string{"tags", "cost", "title"}},
		all: []session.Session{
			{ID: "cheap", Title: "blog work", Labels: []string{"project=blog"}, Cost: 1, HasCost: true},
			{ID: "leak", Title: "project=dotfiles gpt engineer", Labels: []string{"project=dotfiles"}, Cost: 99, HasCost: true},
			{ID: "rich", Title: "blog hd", Labels: []string{"project=blog"}, Cost: 300, HasCost: true},
		},
		meta:      map[string]view.RowMeta{},
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		visual:    -1,
		km:        keys.Build(nil),
	}
	p.enter(mFilter)
	for _, r := range "project=blog" {
		p.handleInsert(ev{t: evRune, r: r})
	}
	p.handleInsert(ev{t: evEnter}) // commit
	if len(p.matches) != 2 {
		t.Fatalf("exact filter should keep 2 blog rows, got %v", p.matches)
	}
	for _, mi := range p.matches {
		if p.all[mi].ID == "leak" {
			t.Fatal("fuzzy leak: project=dotfiles matched project=blog")
		}
	}
	// rows must follow p.all order (the sort), not fuzzy rank
	if p.all[p.matches[0]].ID != "cheap" || p.all[p.matches[1]].ID != "rich" {
		t.Fatalf("committed filter broke sort order: %v %v",
			p.all[p.matches[0]].ID, p.all[p.matches[1]].ID)
	}
}
