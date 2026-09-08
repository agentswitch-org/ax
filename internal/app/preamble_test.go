package app

import (
	"strings"
	"testing"
)

func TestAxPreambleNamesSessionAndHelp(t *testing.T) {
	got := axPreamble("sid-1", "run-a")
	for _, want := range []string{"sid-1", "run-a", "ax help", "ax restart ID", "ax search"} {
		if !strings.Contains(got, want) {
			t.Fatalf("preamble missing %q:\n%s", want, got)
		}
	}
}

func TestAxPreambleOmitsMissingIdentity(t *testing.T) {
	got := axPreamble("", "")
	if strings.Contains(got, "session id is") || strings.Contains(got, "Your run is") {
		t.Fatalf("preamble must not name an empty identity:\n%s", got)
	}
}

func TestWithPreambleOrdersPreambleFirst(t *testing.T) {
	got := withPreamble("sid", "run", "You are a reviewer.")
	if !strings.HasPrefix(got, "# ax\n") {
		t.Fatalf("preamble must come first:\n%s", got)
	}
	if !strings.HasSuffix(got, "You are a reviewer.") {
		t.Fatalf("behavior must follow the preamble:\n%s", got)
	}
	if withPreamble("sid", "run", "  \n") != axPreamble("sid", "run") {
		t.Fatal("blank behavior must yield the preamble alone")
	}
}

func TestPreambleFillsClaudeSystemPromptSlot(t *testing.T) {
	tmpl := "claude --session-id {newid} --model {model} --append-system-prompt {behavior} {args} {task}"
	cmd := fillTemplate(tmpl, map[string]string{
		"newid": "sid", "behavior": withPreamble("sid", "run-a", ""), "task": "do it",
	})
	if !strings.Contains(cmd, "--append-system-prompt '# ax") {
		t.Fatalf("preamble missing from the system-prompt slot:\n%s", cmd)
	}
	if !strings.HasSuffix(strings.TrimSpace(cmd), "do it") && !strings.HasSuffix(strings.TrimSpace(cmd), "'do it'") {
		t.Fatalf("task must stay last:\n%s", cmd)
	}
}
