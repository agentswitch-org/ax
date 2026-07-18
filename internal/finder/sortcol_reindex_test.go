package finder

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

// A reindex can swap in a config whose layout grew a column to the LEFT of the
// sort column (a first tagged/grouped/yolo session auto-inserts one). The sort
// and selection must re-anchor by column KEY: keeping the numeric index used to
// leave the rows sorted by the old column while the ▲/▼ arrow drifted onto its
// neighbor.
func TestApplyReindexKeepsSortColumnByKey(t *testing.T) {
	sessions := []session.Session{{ID: "a"}, {ID: "b"}}
	p := &picker{
		cfg:       config.Config{Columns: []string{"harness", "status", "age", "title"}},
		all:       sessions,
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		visual:    -1,
		km:        keys.Build(nil),
	}
	p.sortCol = view.ColumnIndex(p.cfg, "age")
	p.selCol = p.sortCol
	if p.sortCol != 2 {
		t.Fatalf("setup: age at %d, want 2", p.sortCol)
	}
	p.recompute()

	grown := config.Config{Columns: []string{"harness", "status", "yolo", "tags", "age", "title"}}
	p.applyReindex(reindexResult{sessions: sessions, cfg: grown})

	if got := view.ColumnKey(p.cfg, p.sortCol); got != "age" {
		t.Fatalf("sort column after reindex = %q (index %d), want age", got, p.sortCol)
	}
	if got := view.ColumnKey(p.cfg, p.selCol); got != "age" {
		t.Fatalf("selected column after reindex = %q (index %d), want age", got, p.selCol)
	}

	// A sort column that vanished from the new layout falls back to the default.
	shrunk := config.Config{Columns: []string{"harness", "status", "title"}}
	p.applyReindex(reindexResult{sessions: sessions, cfg: shrunk})
	if p.sortCol < 0 || p.sortCol >= view.NumCols(p.cfg) {
		t.Fatalf("sort column out of range after shrink: %d", p.sortCol)
	}
}
