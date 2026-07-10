package view

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/models"
	"github.com/agentswitch-org/ax/internal/session"
)

// TestRowDimsNonLive pins the picker's visual hierarchy: a live row renders at
// full brightness while every non-live row (crash, a concluded worker, and the
// empty inactive state) is faded to the faint intensity so it recedes. dimRow
// prefixes the whole row with the faint SGR (\x1b[2m), so its presence at the
// head of the line is the signal that a row got the dim treatment.
func TestRowDimsNonLive(t *testing.T) {
	cfg := config.Config{Columns: []string{"name", "title"}}
	db := models.DB{}
	s := session.Session{Name: "alpha", Title: "some work"}
	const dim = "\x1b[2m"

	live := Row(cfg, db, s, RowMeta{State: StateLive}, 0)
	if strings.HasPrefix(live, dim) {
		t.Errorf("live row was dimmed: %q", live)
	}

	nonLive := []struct {
		name string
		m    RowMeta
	}{
		{"crash", RowMeta{State: StateCrash}},
		{"done", RowMeta{Done: true}},    // concluded worker, control-layer state
		{"inactive", RowMeta{State: ""}}, // empty inactive state
	}
	for _, c := range nonLive {
		got := Row(cfg, db, s, c.m, 0)
		if !strings.HasPrefix(got, dim) {
			t.Errorf("%s row not dimmed: %q", c.name, got)
		}
		// The dim is a color/intensity change only: the visible text is unchanged.
		if StripANSI(got) != StripANSI(live) {
			t.Errorf("%s row text differs from live row: %q vs %q", c.name, StripANSI(got), StripANSI(live))
		}
	}
}
