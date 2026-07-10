package view

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
)

// The RUN column (formerly GROUP) resolves under its current key, and the
// deprecated "group" config key still resolves to the same column, so an
// existing `columns = [...,"group",...]` config keeps working.
func TestRunColumnKeyAndDeprecatedGroupAlias(t *testing.T) {
	for _, key := range []string{"run", "group"} {
		cfg := config.Config{Columns: []string{key}}
		got := Columns(cfg, -1, -1, false)
		if got != "RUN" {
			t.Errorf("columns=[%q]: header = %q, want %q", key, got, "RUN")
		}
		if NumCols(cfg) != 1 {
			t.Errorf("columns=[%q]: NumCols = %d, want 1", key, NumCols(cfg))
		}
	}
}

// WithGroupColumns uses the real session handle, not the run id, for the
// automatic identity column added to run rows.
func TestWithGroupColumnsInsertsSessionIDNotRun(t *testing.T) {
	cfg := config.Config{Columns: []string{"status", "activity", "title"}}
	got := WithGroupColumns(cfg)
	joined := "," + strings.Join(got.Columns, ",") + ","
	for _, key := range []string{"name", "id"} {
		if !strings.Contains(joined, ","+key+",") {
			t.Fatalf("WithGroupColumns columns = %v, want %q", got.Columns, key)
		}
	}
	if strings.Contains(joined, ",run,") || strings.Contains(joined, ",group,") {
		t.Fatalf("WithGroupColumns columns = %v, want no automatic run column", got.Columns)
	}
	if header := Columns(got, -1, -1, false); !strings.Contains(header, "ID") || strings.Contains(header, "RUN") {
		t.Fatalf("WithGroupColumns header = %q, want ID and no RUN", header)
	}
}
