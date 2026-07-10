package app

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/retention"
	"github.com/agentswitch-org/ax/internal/state"
)

// TestLocalReportWorkerAskStaysBlockedNotConcluded: the owner report is the
// federated source of truth. A worker (non-empty Parent) with a pending ask is
// blocked on input until a real Done/Failed marker arrives, so lifecycle must
// not report it as concluded.
func TestLocalReportWorkerAskStaysBlockedNotConcluded(t *testing.T) {
	home := isolate(t)

	const worker = "00000000-0000-0000-0000-000000000aaa"
	const top = "11111111-0000-0000-0000-000000000bbb"

	writeClaudeTranscript(t, home, worker, "worker result")
	writeClaudeTranscript(t, home, top, "top result")
	if err := meta.Save(worker, meta.Meta{Mode: "interactive", Task: "do it", Parent: "coord-1"}); err != nil {
		t.Fatal(err)
	}
	if err := meta.Save(top, meta.Meta{Mode: "interactive", Task: "do it"}); err != nil {
		t.Fatal(err)
	}
	// Both blocked on a human ask.
	if err := ask.Save(worker, ask.Pending{Question: "which?"}); err != nil {
		t.Fatal(err)
	}
	if err := ask.Save(top, ask.Pending{Question: "which?"}); err != nil {
		t.Fatal(err)
	}

	type snap struct {
		waiting   string
		done      bool
		parent    string
		lifecycle string
	}
	byID := map[string]snap{}
	rep := App{}.localReport("", retention.ActiveOnly)
	for _, s := range rep.Sessions {
		byID[s.ID] = snap{s.Waiting, s.Done, s.Parent, s.Lifecycle}
	}

	w, ok := byID[worker]
	if !ok {
		t.Fatalf("worker session missing from report; got %d sessions", len(byID))
	}
	if w.parent == "" {
		t.Fatal("worker lost its Parent in the report")
	}
	if w.waiting != "input" || w.done || w.lifecycle == state.LifecycleConcluded {
		t.Fatalf("worker report waiting=%q done=%v lifecycle=%q, want blocked and not concluded", w.waiting, w.done, w.lifecycle)
	}

	if tp := byID[top]; tp.waiting != "input" || tp.done {
		t.Fatalf("top-level report waiting=%q done=%v, want needs-you input", tp.waiting, tp.done)
	}
}
