package state

import "testing"

func TestIsYolo(t *testing.T) {
	yes := []string{
		"claude --session-id 'x' --dangerously-skip-permissions 'do the task'",
		"codex --yolo 'task'",
		"codex --dangerously-bypass-approvals-and-sandbox",
		"codex exec --skip-git-repo-check --sandbox danger-full-access -c approval_policy=\"never\" 'task'",
		"claude --permission-mode bypassPermissions",
		// an odd apostrophe in the quoted text must not hide a real flag
		`claude --append-system-prompt 'You'\''re the orchestrator' --dangerously-skip-permissions 'task'`,
	}
	no := []string{
		"claude --session-id 'x' 'document the --dangerously-skip-permissions flag'",
		"pi --session 'y'",
		"claude 'is --yolo safe?'",
	}
	for _, c := range yes {
		if !IsYolo(c) {
			t.Fatalf("IsYolo(%q) = false", c)
		}
	}
	for _, c := range no {
		if IsYolo(c) {
			t.Fatalf("IsYolo(%q) = true", c)
		}
	}
}
