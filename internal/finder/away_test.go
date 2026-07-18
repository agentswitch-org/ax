package finder

import (
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

func awayPicker(sessions []session.Session, meta map[string]view.RowMeta) *picker {
	return &picker{
		cfg:          config.Config{Columns: []string{"status", "name", "title"}},
		all:          sessions,
		meta:         meta,
		collapsed:    map[string]bool{},
		marks:        map[string]bool{},
		visual:       -1,
		km:           keys.Build(nil),
		previewCache: map[string][]string{},
		previewRev:   map[string]previewRevision{},
		fetching:     map[string]bool{},
		rowText:      map[string]string{},
	}
}

// Sessions that concluded or started needing you since the previous open get
// the ● mark and the digest notice; previewing a row clears its mark.
func TestComputeAway(t *testing.T) {
	now := time.Now()
	sessions := []session.Session{
		{ID: "fresh-done", Last: now},
		{ID: "old-done", Last: now.Add(-3 * time.Hour)},
		{ID: "needs", Last: now},
		{ID: "quiet", Last: now},
	}
	meta := map[string]view.RowMeta{
		"fresh-done": {Done: true, TerminalAt: now.Add(-time.Minute)},
		"old-done":   {Done: true, TerminalAt: now.Add(-3 * time.Hour)},
		"needs":      {State: view.StateLive, Waiting: "input"},
		"quiet":      {State: view.StateLive},
	}
	p := awayPicker(sessions, meta)
	p.prevOpen = now.Add(-time.Hour)
	p.computeAway()

	if !p.newKeys["fresh-done"] || !p.newKeys["needs"] {
		t.Fatalf("fresh conclusions/attention not marked: %v", p.newKeys)
	}
	if p.newKeys["old-done"] || p.newKeys["quiet"] {
		t.Fatalf("stale/quiet sessions marked: %v", p.newKeys)
	}
	if p.notice == "" {
		t.Fatal("no while-you-were-away digest")
	}

	// Previewing the row clears its dot.
	p.recompute()
	p.cursor = p.matchIndexOf("fresh-done")
	p.buildPreview()
	if p.newKeys["fresh-done"] {
		t.Fatal("previewing did not clear the mark")
	}
}

// Rows waiting on a human pin to the top of the plain browse view, above the
// sort order; ranked (filter) and grouped views are left alone.
func TestPinAttention(t *testing.T) {
	sessions := []session.Session{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}
	meta := map[string]view.RowMeta{
		"a": {State: view.StateLive},
		"b": {State: view.StateLive, Waiting: "input"},
		"c": {Done: true},
	}
	p := awayPicker(sessions, meta)
	p.recompute()
	ids := testMatchIDs(p)
	if len(ids) != 3 || ids[0] != "b" {
		t.Fatalf("attention row not pinned first: %v", ids)
	}
}

// The all-columns filter matches text from ANY column without selecting it.
func TestGlobalFilter(t *testing.T) {
	sessions := []session.Session{
		{ID: "x", Title: "fix the flaky test", Dir: "/src/blog"},
		{ID: "y", Title: "write docs", Dir: "/src/site"},
	}
	meta := map[string]view.RowMeta{"x": {}, "y": {}}
	p := awayPicker(sessions, meta)
	p.rowText = map[string]string{}
	p.enterAllFilter()
	p.query = "blog"
	p.recompute()
	ids := testMatchIDs(p)
	if len(ids) != 1 || ids[0] != "x" {
		t.Fatalf("global filter on dir text = %v, want [x]", ids)
	}
	if p.filterColumnLabel() != "all" {
		t.Fatalf("filter label = %q, want all", p.filterColumnLabel())
	}
}
