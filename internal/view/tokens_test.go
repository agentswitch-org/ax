package view

import (
	"sort"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/models"
	"github.com/agentswitch-org/ax/internal/session"
)

func tokenCellFor(s session.Session) string {
	col := registryByKey("tokens")
	return col.cell(s, models.DB{}, RowMeta{}, col.width, 0)
}

func registryByKey(key string) column {
	for _, c := range registry {
		if c.key == key {
			return c
		}
	}
	panic("column not found: " + key)
}

func TestTokensCellRendering(t *testing.T) {
	cases := []struct {
		name        string
		s           session.Session
		wantContain string
		wantAbsent  string
	}{
		{
			name:        "zero tokens shows dash",
			s:           session.Session{},
			wantContain: "-",
		},
		{
			name:        "small counts",
			s:           session.Session{InTok: 420, OutTok: 88},
			wantContain: "420",
		},
		{
			name:        "k suffix for thousands",
			s:           session.Session{InTok: 45000, OutTok: 12000},
			wantContain: "45k",
		},
		{
			name:        "slash separator",
			s:           session.Session{InTok: 45000, OutTok: 12000},
			wantContain: "/",
		},
		{
			name:        "M suffix for millions",
			s:           session.Session{InTok: 1_500_000, OutTok: 452_000},
			wantContain: "1.5M",
		},
	}
	for _, c := range cases {
		got := StripANSI(tokenCellFor(c.s))
		if !strings.Contains(got, c.wantContain) {
			t.Errorf("%s: token cell = %q, want it to contain %q", c.name, got, c.wantContain)
		}
		if c.wantAbsent != "" && strings.Contains(got, c.wantAbsent) {
			t.Errorf("%s: token cell = %q, must not contain %q", c.name, got, c.wantAbsent)
		}
	}
}

func TestTokensCellSortByTotal(t *testing.T) {
	col := registryByKey("tokens")
	sessions := []session.Session{
		{ID: "c", InTok: 100_000, OutTok: 50_000}, // 150k
		{ID: "a", InTok: 10_000, OutTok: 5_000},   // 15k
		{ID: "b", InTok: 50_000, OutTok: 20_000},  // 70k
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		return col.less(sessions[i], sessions[j], models.DB{}, nil)
	})
	order := sessions[0].ID + sessions[1].ID + sessions[2].ID
	if order != "abc" {
		t.Errorf("token sort order = %q, want %q", order, "abc")
	}
}

func TestTokensInDefaultOrder(t *testing.T) {
	found := false
	for _, k := range defaultOrder {
		if k == "tokens" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("tokens column not present in defaultOrder: %v", defaultOrder)
	}
}

func TestEffortColumnCell(t *testing.T) {
	col := registryByKey("effort")

	got := StripANSI(col.cell(session.Session{Effort: "high"}, models.DB{}, RowMeta{}, col.width, 0))
	if got != "high" {
		t.Errorf("effort cell with Effort=%q = %q, want %q", "high", got, "high")
	}

	got = StripANSI(col.cell(session.Session{}, models.DB{}, RowMeta{}, col.width, 0))
	if got != "" {
		t.Errorf("effort cell with empty Effort = %q, want empty string", got)
	}
}

func TestEffortInDefaultOrder(t *testing.T) {
	found := false
	for _, k := range defaultOrder {
		if k == "effort" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("effort column not present in defaultOrder: %v", defaultOrder)
	}
}
