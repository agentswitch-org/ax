package retention

import (
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
)

func TestApplyAutoArchivesOnlySafeEphemeralConcluded(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	ret := config.Retention{AutoRetire: true, RetainAfter: "10m", PruneCrashed: true}
	sessions := []session.Session{
		{ID: "ephemeral", Parent: "root", Last: now.Add(-20 * time.Minute)},
		{ID: "durable", Last: now.Add(-20 * time.Minute)},
		{ID: "live", Parent: "root", Last: now.Add(-20 * time.Minute)},
		{ID: "pending", Parent: "root", Last: now.Add(-20 * time.Minute)},
	}
	if err := ask.Save("pending", ask.Pending{Question: "wait"}); err != nil {
		t.Fatal(err)
	}
	rt := map[string]state.Runtime{
		"ephemeral": {Done: true},
		"durable":   {Done: true},
		"live":      {State: state.Live, Done: true},
		"pending":   {Done: true},
	}
	n, err := ApplyAuto(ret, sessions, rt, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ApplyAuto archived %d sessions, want 1", n)
	}
	if !sessions[0].Archived || !meta.Load("ephemeral").Archived {
		t.Fatal("ephemeral concluded worker was not archived in memory and metadata")
	}
	for _, id := range []string{"durable", "live", "pending"} {
		if meta.Load(id).Archived {
			t.Fatalf("%s must not auto-archive", id)
		}
	}
}

func TestPruneCandidatesSafetyAndScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	ret := config.Retention{AutoRetire: true, RetainAfter: "10m", PruneCrashed: true}
	sessions := []session.Session{
		{ID: "done", Group: "run1", Parent: "root", Last: now.Add(-2 * time.Hour)},
		{ID: "crash", Group: "run1", Parent: "root", Last: now.Add(-2 * time.Hour)},
		{ID: "new", Group: "run1", Parent: "root", Last: now.Add(-time.Minute)},
		{ID: "durable", Group: "run1", Last: now.Add(-2 * time.Hour)},
		{ID: "live", Group: "run1", Parent: "root", Last: now.Add(-2 * time.Hour)},
		{ID: "pending", Group: "run1", Parent: "root", Last: now.Add(-2 * time.Hour)},
		{ID: "other-run", Group: "run2", Parent: "root", Last: now.Add(-2 * time.Hour)},
	}
	if err := ask.Save("pending", ask.Pending{Question: "wait"}); err != nil {
		t.Fatal(err)
	}
	rt := map[string]state.Runtime{
		"done":      {Done: true},
		"crash":     {State: state.Crash},
		"new":       {Done: true},
		"durable":   {Done: true},
		"live":      {State: state.Live},
		"pending":   {Done: true},
		"other-run": {Done: true},
	}
	got := PruneCandidates(ret, sessions, rt, "run1", 10*time.Minute, now)
	ids := map[string]bool{}
	for _, c := range got {
		ids[c.Session.ID] = true
	}
	if len(ids) != 2 || !ids["done"] || !ids["crash"] {
		t.Fatalf("PruneCandidates ids = %v, want done+crash only", ids)
	}
}
