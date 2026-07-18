package finder

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

// The z toggle snaps the selected column to its widest cell content, survives
// a reindex config swap, and restores the default width on the second press.
func TestToggleColExpand(t *testing.T) {
	long := session.Session{ID: "a", Dir: "/very/long/path/that/never/fits/in/thirty/cells/of/width/at/all"}
	p := &picker{
		cfg:       config.Config{Columns: []string{"harness", "dir", "title"}},
		all:       []session.Session{long, {ID: "b", Dir: "/tmp"}},
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		visual:    -1,
		km:        keys.Build(nil),
	}
	p.selCol = view.ColumnIndex(p.cfg, "dir")
	p.recompute()

	p.toggleColExpand()
	w := p.cfg.ColWidths["dir"]
	if w <= 30 { // registry default dir width
		t.Fatalf("expanded dir width = %d, want > default 30", w)
	}

	// A reindex swaps the config in; the snap must survive.
	p.applyReindex(reindexResult{sessions: p.all, cfg: config.Config{Columns: []string{"harness", "dir", "title"}}})
	if got := p.cfg.ColWidths["dir"]; got != w {
		t.Fatalf("expanded width after reindex = %d, want %d", got, w)
	}

	// Second press restores the default width.
	p.selCol = view.ColumnIndex(p.cfg, "dir")
	p.toggleColExpand()
	if got, ok := p.cfg.ColWidths["dir"]; ok && got != 0 {
		t.Fatalf("width override after un-snap = %d, want none", got)
	}
}
