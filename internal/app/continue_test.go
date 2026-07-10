package app

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/session"
)

// continue's argv is <id> then the task and flags: the id is the first bare word,
// the rest parses as a launch (task + flags).
func TestParseContinueArgs(t *testing.T) {
	id, o, err := parseContinue([]string{"sess-1", "fix", "the", "bug", "--model", "opus", "--wait"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "sess-1" {
		t.Fatalf("id: want sess-1, got %q", id)
	}
	if o.task != "fix the bug" {
		t.Fatalf("task: want %q, got %q", "fix the bug", o.task)
	}
	if o.model != "opus" || !o.wait {
		t.Fatalf("flags not parsed: model=%q wait=%v", o.model, o.wait)
	}
}

func TestParseContinueRejectsMissingIDAndTask(t *testing.T) {
	if _, _, err := parseContinue(nil); err == nil {
		t.Fatal("empty argv must error")
	}
	if _, _, err := parseContinue([]string{"--model", "opus"}); err == nil {
		t.Fatal("a leading flag (no id) must error")
	}
	if _, _, err := parseContinue([]string{"sess-1"}); err == nil {
		t.Fatal("an id with no task must error")
	}
	if _, _, err := parseContinue([]string{"sess-1", "   "}); err == nil {
		t.Fatal("a blank task must error")
	}
}

// The harness-resume decision: claude resumes with input (watched and headless),
// a harness with no resume-with-input form degrades gracefully, and a harness with
// only an interactive form refuses --wait with a distinct message.
func TestResumeInputTemplateDecision(t *testing.T) {
	claude := config.Default().Harnesses[0] // claude carries both forms by default
	if claude.Name != "claude" {
		t.Fatalf("expected claude first in defaults, got %q", claude.Name)
	}

	tmpl, mode, err := resumeInputTemplate(claude, false)
	if err != nil || mode != "interactive" || !strings.Contains(tmpl, "--resume {id}") || !strings.Contains(tmpl, "{task}") {
		t.Fatalf("claude watched continue: tmpl=%q mode=%q err=%v", tmpl, mode, err)
	}
	tmpl, mode, err = resumeInputTemplate(claude, true)
	if err != nil || mode != "headless" || !strings.Contains(tmpl, "-p") || !strings.Contains(tmpl, "--resume {id}") {
		t.Fatalf("claude headless continue: tmpl=%q mode=%q err=%v", tmpl, mode, err)
	}

	// A harness with neither form cannot continue: the sentinel drives the
	// attach-and-send / launch-fresh message.
	bare := config.Harness{Name: "codex", Format: "codex"}
	if _, _, err := resumeInputTemplate(bare, false); !errors.Is(err, errContinueUnsupported) {
		t.Fatalf("unsupported harness must return errContinueUnsupported, got %v", err)
	}
	if _, _, err := resumeInputTemplate(bare, true); !errors.Is(err, errContinueUnsupported) {
		t.Fatalf("unsupported harness (headless) must return errContinueUnsupported, got %v", err)
	}

	// Interactive-only harness: a --wait continue is refused with a non-sentinel
	// error (telling the user to drop --wait), not the unsupported path.
	interOnly := config.Harness{Name: "x", ResumeInput: "resume {id} {task}"}
	if _, _, err := resumeInputTemplate(interOnly, true); err == nil || errors.Is(err, errContinueUnsupported) {
		t.Fatalf("interactive-only harness must refuse --wait with a distinct error, got %v", err)
	}
	if tmpl, mode, err := resumeInputTemplate(interOnly, false); err != nil || mode != "interactive" || tmpl == "" {
		t.Fatalf("interactive-only harness must resume watched: tmpl=%q mode=%q err=%v", tmpl, mode, err)
	}
}

// ax continue accepts --task-file so multi-line prompts skip shell-quoting.
func TestParseContinueTaskFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "task*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("fix the regression\nstep by step"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	id, o, err := parseContinue([]string{"sess-1", "--task-file", f.Name()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "sess-1" {
		t.Fatalf("id = %q, want sess-1", id)
	}
	if o.task != "fix the regression\nstep by step" {
		t.Fatalf("task = %q", o.task)
	}
}

// ax continue --task-file - reads from stdin.
func TestParseContinueTaskFileStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig; r.Close() }()

	if _, err := w.Write([]byte("continue task from stdin")); err != nil {
		t.Fatal(err)
	}
	w.Close()

	id, o, err := parseContinue([]string{"sess-1", "--task-file", "-"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "sess-1" || o.task != "continue task from stdin" {
		t.Fatalf("id=%q task=%q", id, o.task)
	}
}

// The claude continue command wires the documented resume mechanism with the new
// task as input: `claude --resume <id> ... <task>`, quoted, with the model flag
// dropped when unknown.
func TestContinueCommandWiring(t *testing.T) {
	claude := config.Default().Harnesses[0]
	s := session.Session{ID: "abc-123", Dir: "/home/agent/proj", Model: "opus"}

	tmpl, _, err := resumeInputTemplate(claude, false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := fillTemplate(tmpl, map[string]string{
		"id": s.ID, "dir": s.Dir, "model": s.Model, "task": "review the diff", "args": "--dangerously-skip-permissions",
	})
	if !strings.Contains(cmd, "--resume 'abc-123'") && !strings.Contains(cmd, "--resume abc-123") {
		t.Fatalf("continue must resume the existing session id: %q", cmd)
	}
	if !strings.Contains(cmd, "review the diff") {
		t.Fatalf("continue must deliver the new task as input: %q", cmd)
	}

	// A synthetic/unknown model drops the --model flag rather than passing a
	// placeholder claude cannot resume with.
	dropped := fillTemplate(tmpl, map[string]string{"id": s.ID, "dir": s.Dir, "model": "", "task": "go", "args": ""})
	if strings.Contains(dropped, "--model") {
		t.Fatalf("an unknown model must drop the --model flag: %q", dropped)
	}
}
