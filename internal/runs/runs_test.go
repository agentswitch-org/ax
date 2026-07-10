package runs

import (
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/session"
)

// Build stamps Started from the root session's Created time, so a reader can
// derive run duration (Concluded - Started) without re-reading transcripts.
func TestBuildSetsStartedFromRoot(t *testing.T) {
	created := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	sessions := []session.Session{
		{ID: "root", Created: created, Task: "ship it"},
		{ID: "worker", Parent: "root", Created: created.Add(time.Minute)},
	}
	r := Build("g1", Success, sessions, func(session.Session) float64 { return 0 }, func(string) string { return "" })
	if !r.Started.Equal(created) {
		t.Fatalf("Started = %v, want %v", r.Started, created)
	}
}
