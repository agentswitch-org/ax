package view

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/session"
)

func TestTagColumns(t *testing.T) {
	cfg := config.Config{Columns: []string{"tags", "tag:workstream", "title"}}
	s := session.Session{
		Title:  "fix the stream",
		Labels: []string{"pinned", "workstream=blog"},
	}
	// Too wide for the 18-cell column: the overflowing label truncates with an
	// ellipsis, keys intact and unabbreviated.
	row := StripANSI(Row(cfg, nil, s, RowMeta{}, 0))
	if !strings.Contains(row, "pinned workstream…") {
		t.Fatalf("tags column should ellipsize, got: %q", row)
	}
	// A long single kv label keeps its key and clips the value.
	if got := StripANSI(tagsCell([]string{"workstream=a-very-long-value"}, 14)); got != "workstream=a-…" {
		t.Fatalf("clipped kv = %q", got)
	}
	// Fits: the key renders (dimmed) alongside the value.
	if got := StripANSI(tagsCell([]string{"ws=hd"}, 18)); got != "ws=hd" {
		t.Fatalf("tags cell (fits) = %q", got)
	}
	// the per-key column shows just the value
	if !strings.Contains(row, "blog  ") && !strings.HasSuffix(row, "blog") {
		t.Fatalf("tag:workstream column missing value: %q", row)
	}
	head := StripANSI(Columns(cfg, -1, -1, false))
	if !strings.Contains(head, "TAGS") || !strings.Contains(head, "WORKSTREAM") {
		t.Fatalf("headers missing: %q", head)
	}
}

func TestTagColumnSort(t *testing.T) {
	cfg := config.Config{Columns: []string{"tag:ws"}}
	ss := []session.Session{
		{ID: "b", Labels: []string{"ws=beta"}},
		{ID: "a", Labels: []string{"ws=alpha"}},
	}
	Sort(cfg, ss, nil, 0, false, map[string]RowMeta{})
	if ss[0].ID != "a" {
		t.Fatalf("sort by tag value: got %s first", ss[0].ID)
	}
}
