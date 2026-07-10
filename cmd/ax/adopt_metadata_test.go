package main

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/session"
)

func TestAdoptControlMetaBackfillsParentFromEnv(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("AX_RUN", "run1")
	t.Setenv("AX_PARENT", "coord")
	t.Setenv("AX_LABELS", "role=worker\nproject=ax")
	if err := meta.Save("launch-1", meta.Meta{Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}

	got := adoptControlMeta("launch-1")

	if got.Group != "run1" || got.Parent != "coord" || got.Origin != "agent" {
		t.Fatalf("group=%q parent=%q origin=%q, want run1/coord/agent", got.Group, got.Parent, got.Origin)
	}
	if session.LabelValue(got.Labels, "role") != "worker" || session.LabelValue(got.Labels, "project") != "ax" {
		t.Fatalf("labels = %v, want role=worker and project=ax", got.Labels)
	}
}

func TestAdoptControlMetaDoesNotUseOwnSessionIDAsParent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("AX_RUN", "run1")
	t.Setenv("AX_PARENT", "")
	t.Setenv("AX_SESSION_ID", "launch-1")

	got := adoptControlMeta("launch-1")

	if got.Parent != "" {
		t.Fatalf("parent = %q, want empty", got.Parent)
	}
}
