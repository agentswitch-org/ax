package view

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
)

// TestStatusCellStates pins that the merged status column tells apart the four
// live states a concluded worker must never be confused between: working,
// waiting-for-input, done, and detached (plus the plain idle it must not read as).
func TestStatusCellStates(t *testing.T) {
	cases := []struct {
		name string
		m    RowMeta
		want string
	}{
		{"working", RowMeta{State: StateLive, Activity: Working}, "working"},
		{"needs you", RowMeta{State: StateLive, Waiting: "input"}, "needs you"},
		{"done", RowMeta{State: StateLive, Done: true}, "done"},
		{"detached", RowMeta{State: StateLive, Detached: true}, "detached"},
		{"idle", RowMeta{State: StateLive}, "idle"},
	}
	for _, c := range cases {
		got := StripANSI(statusCell(c.m, 0))
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: statusCell = %q, want it to contain %q", c.name, got, c.want)
		}
	}

	// A concluded worker must not read as a bare idle, and done outranks a stray
	// idle activity (a done row's activity falls back to idle).
	done := StripANSI(statusCell(RowMeta{State: StateLive, Activity: Idle, Done: true}, 0))
	if done == "idle" || !strings.Contains(done, "done") {
		t.Fatalf("done worker rendered as %q, want a distinct done state", done)
	}
}

func TestLifecycleCellDisplayPhase(t *testing.T) {
	got := StripANSI(lifecycleCell(PhaseLiveDoneResident))
	if got != "done-resident" {
		t.Fatalf("done resident lifecycle cell = %q, want done-resident", got)
	}
	row := StripANSI(Row(config.Config{Columns: []string{"lifecycle"}}, nil, session.Session{ID: "s1"}, RowMeta{
		Lifecycle: state.LifecycleLive, DisplayPhase: PhaseLiveDoneResident,
	}, 0))
	if !strings.Contains(row, "done-resident") || strings.Contains(row, " live ") {
		t.Fatalf("row lifecycle display is not honest: %q", row)
	}
}
