package state

import (
	"testing"
	"time"
)

func TestLifecycleClassifier(t *testing.T) {
	tests := []struct {
		name string
		rt   Runtime
		want string
	}{
		{"live", Runtime{State: Live}, LifecycleLive},
		{"done beats live", Runtime{State: Live, Done: true}, LifecycleConcluded},
		{"failed beats live", Runtime{State: Live, Failed: true}, LifecycleConcluded},
		{"done", Runtime{Done: true}, LifecycleConcluded},
		{"failed", Runtime{Failed: true}, LifecycleConcluded},
		{"crashed", Runtime{State: Crash}, LifecycleCrashed},
		{"dormant", Runtime{}, LifecycleDormant},
	}
	for _, tt := range tests {
		if got := Lifecycle(tt.rt); got != tt.want {
			t.Fatalf("%s: Lifecycle = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestDisplayPhaseClassifier(t *testing.T) {
	tests := []struct {
		name string
		rt   Runtime
		want string
	}{
		// A freshly launched codex worker: process alive, no genuine terminal
		// marker written (Done stays false because codexTurnEnd no longer concludes
		// on the empty boot turn). It must read live-working, never concluded.
		{"fresh live worker not concluded", Runtime{State: Live, Activity: Working, Done: false}, DisplayLiveWorking},
		{"live working", Runtime{State: Live, Activity: Working}, DisplayLiveWorking},
		{"live waiting", Runtime{State: Live, Activity: Idle}, DisplayLiveWaiting},
		{"live done resident", Runtime{State: Live, Done: true}, DisplayLiveDoneResident},
		{"live failed resident", Runtime{State: Live, Failed: true}, DisplayLiveDoneResident},
		{"concluded", Runtime{Done: true}, DisplayConcluded},
		{"crashed", Runtime{State: Crash}, DisplayCrashed},
		{"dormant", Runtime{}, DisplayDormant},
	}
	for _, tt := range tests {
		if got := DisplayPhase(tt.rt); got != tt.want {
			t.Fatalf("%s: DisplayPhase = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestShouldAutoRetire(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	base := AutoRetireInput{
		Parent:      "parent",
		Runtime:     Runtime{Done: true},
		Last:        now.Add(-20 * time.Minute),
		Now:         now,
		RetainAfter: 10 * time.Minute,
	}
	if !ShouldAutoRetire(base) {
		t.Fatal("concluded old ephemeral worker should auto-retire")
	}
	for _, tt := range []struct {
		name string
		edit func(*AutoRetireInput)
	}{
		{"durable", func(in *AutoRetireInput) { in.Parent = "" }},
		{"live", func(in *AutoRetireInput) { in.Runtime = Runtime{State: Live, Done: true} }},
		{"pending ask", func(in *AutoRetireInput) { in.PendingAsk = true }},
		{"too new", func(in *AutoRetireInput) { in.Last = now.Add(-time.Minute) }},
		{"already archived", func(in *AutoRetireInput) { in.Archived = true }},
		{"not concluded", func(in *AutoRetireInput) { in.Runtime = Runtime{} }},
	} {
		in := base
		tt.edit(&in)
		if ShouldAutoRetire(in) {
			t.Fatalf("%s should not auto-retire", tt.name)
		}
	}
}
