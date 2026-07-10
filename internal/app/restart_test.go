package app

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/runs"
)

// A restart marks the group suppressed before it kills the old root; the dying
// wrapper's ConcludeRun then swallows exactly that one conclusion (so the run is
// not recorded as gave_up) and clears the one-shot marker, so the relaunched
// session concludes normally later.
func TestConcludeRunSuppressed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const group = "grp-restart"

	runs.Suppress(group)
	if !runs.Suppressed(group) {
		t.Fatal("Suppress did not set the marker")
	}

	// The dying wrapper concludes: no record is written and the marker is cleared.
	var a App
	a.ConcludeRun(group)
	if runs.Exists(group) {
		t.Fatal("a suppressed conclusion must not write a run record")
	}
	if runs.Suppressed(group) {
		t.Fatal("the one-shot suppression marker was not cleared")
	}
}
