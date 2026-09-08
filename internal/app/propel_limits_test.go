package app

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/state"
)

func TestPropelledStopIsProvisionalUntilPumpVerdict(t *testing.T) {
	isolate(t)
	t.Setenv("AX_SESSION_ID", fixtureID)
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	prior := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = prior; input.Close() })
	if err := meta.Save(fixtureID, meta.Meta{
		Task: "fix it", Mode: "interactive", KeepLive: true, CloseOnDone: true,
		Spec: &meta.Spec{SelfPropel: true},
	}); err != nil {
		t.Fatal(err)
	}
	App{}.HookState([]string{"stop"})
	if state.Terminal(fixtureID) {
		t.Fatal("Stop hook prematurely completed the task")
	}
	if got, _ := state.HookState(fixtureID); got != state.TurnEnded {
		t.Fatalf("hook = %q", got)
	}
	// Avoid a real close-on-done process kill when applying the final verdict.
	meta.Update(fixtureID, func(m *meta.Meta) { m.CloseOnDone = false })
	ConcludePropel(fixtureID, "automatic continuation limit reached (6/6)")
	m := meta.Load(fixtureID)
	if !state.Failed(fixtureID) || resultOutcome(fixtureID, m) != "failure" || !strings.Contains(m.FailReason, "6/6") {
		t.Fatalf("cap did not produce a durable failure: %+v", m)
	}
	if got := waitFor([]string{fixtureID}, true, time.Second, time.Millisecond); got != 1 {
		t.Fatalf("wait exit = %d", got)
	}
}

func TestPropelCapOverridesLegacyDoneMarker(t *testing.T) {
	isolate(t)
	meta.Save(fixtureID, meta.Meta{Task: "fix it", Mode: "interactive", KeepLive: true})
	state.WriteHook(fixtureID, "done")
	ConcludePropel(fixtureID, "budget exhausted")
	if !state.Failed(fixtureID) || meta.Load(fixtureID).FailReason != "budget exhausted" {
		t.Fatal("legacy provisional success survived a failed pump verdict")
	}
}

func TestRemoteLaunchPreservesContinuationSafety(t *testing.T) {
	o := launchOpts{selfPropel: true, propelMaxAutoTurns: 4, propelMaxIdle: 2,
		propelPrompt: "repair the failed check", propelDone: "./check.sh", propelWatch: "artifact.txt", propelBackoff: time.Second}
	argv := remoteLaunchArgv("claude", o, false)
	// Replace the stdin task transport with a literal task for this round trip.
	got, err := parseLaunch(append([]string{"task"}, argv[3:]...))
	if err != nil {
		t.Fatal(err)
	}
	if !got.selfPropel || got.propelMaxAutoTurns != 4 || got.propelMaxIdle != 2 ||
		got.propelDone != o.propelDone || got.propelPrompt != o.propelPrompt || got.propelWatch != o.propelWatch || got.propelBackoff != o.propelBackoff {
		t.Fatalf("remote launch dropped safety policy: %+v", got)
	}
}

func TestAutomaticLimitRejectsUnlimitedAndInvalidValues(t *testing.T) {
	for _, v := range []string{"0", "-1", "many", ""} {
		if _, err := parseLaunch([]string{"task", "--max-auto-turns", v}); err == nil {
			t.Fatalf("accepted %q", v)
		}
	}
}
