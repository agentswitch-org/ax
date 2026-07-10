package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/fence"
	"github.com/agentswitch-org/ax/internal/shell"
)

// FenceCheck is the PreToolUse classifier a fenced session runs (`ax
// fence-check`), installed by fence.Apply. It reads the harness's tool-call JSON
// on stdin and answers with a hook permission decision: a read-only tool (Read,
// Grep, ...) is allowed, a Bash command is allowed only if fence.Classify passes
// it, and everything else (Write, Edit, Task, mcp__*) is denied. The explicit
// allow matters because a fenced session may run headless (`claude -p`), where a tool
// that would otherwise prompt has no human to answer it. This hook is the fence's
// authority; the settings deny-list is only a backstop.
func (a App) FenceCheck(args []string) {
	var payload struct {
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			Command  string `json:"command"`
			FilePath string `json:"file_path"`
		} `json:"tool_input"`
	}
	json.NewDecoder(os.Stdin).Decode(&payload) // best-effort; a bad payload denies below

	// AX_WRITE is the newline-separated write scope: the globs a fenced session may
	// write into, set by the launcher. Empty (or unset) means no file writes are
	// allowed (the --no-write case).
	writeGlobs := splitWriteGlobs(os.Getenv("AX_WRITE"))
	// AX_NO_SUBAGENTS=1 bars this session from the sub-agent spawn tools, so it must
	// delegate via `ax claude ...`. Set by userland (e.g. a role recipe).
	noSubagents := os.Getenv("AX_NO_SUBAGENTS") == "1"
	wd, _ := os.Getwd()

	d := fenceDecide(payload.ToolName, payload.ToolInput.Command, payload.ToolInput.FilePath, writeGlobs, wd, noSubagents)

	decision := "deny"
	if d.Allow {
		decision = "allow"
	}
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       decision,
			"permissionDecisionReason": d.Reason,
		},
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

// fenceDecide is the pure decision for one tool call under the write fence. A
// Bash command is allowed only when fence.Classify passes it; a --no-subagents
// session is barred from the sub-agent spawn tools (Task/TaskCreate/Agent) and
// must delegate via `ax claude ...`; a session WITHOUT --no-subagents may spawn
// them (allowed explicitly so it does not hit the default write-deny); the
// read-only tools are allowed; a file-write is gated by the write scope (allowed
// only for a path matching one of writeGlobs; empty writeGlobs denies every
// write, the --no-write case); everything else is a write and denied.
func fenceDecide(toolName, command, filePath string, writeGlobs []string, wd string, noSubagents bool) fence.Decision {
	switch {
	case toolName == "Bash":
		cfg, _ := config.Load()
		return fence.ClassifyWith(command, cfg.Fence.Allow)
	case noSubagents && fence.IsSubagentTool(toolName):
		return fence.Decision{Allow: false, Reason: "ax fence: this session must delegate via `ax claude ...`, not the Task tool. A Task sub-agent runs outside ax: untracked, unattachable, and unfenced."}
	case fence.IsSubagentTool(toolName):
		return fence.Decision{Allow: true}
	case fence.AllowedTool(toolName):
		return fence.Decision{Allow: true}
	case fence.IsWriteTool(toolName):
		// Write-scope gate: a file-write tool is allowed only for a path matching
		// one of the write globs (no traversal, no symlink escape), so the session
		// can write inside its granted scope; every other target stays blocked.
		if fence.WriteAllowed(writeGlobs, wd, filePath) {
			return fence.Decision{Allow: true}
		}
		if len(writeGlobs) == 0 {
			return fence.Decision{Allow: false, Reason: "ax fence: " + toolName + " is a write; this session cannot write (--no-write). Delegate writes to a worker (ax <harness> \"...\")."}
		}
		return fence.Decision{Allow: false, Reason: "ax fence: " + toolName + " may only write matching: " + strings.Join(writeGlobs, ", ") + "; " + filePath + " does not match. Delegate other writes to a worker."}
	default:
		return fence.Decision{Allow: false, Reason: "ax fence: " + toolName + " is a write; this session cannot write. Run writes in another session (ax <harness> \"...\")."}
	}
}

// splitWriteGlobs parses the newline-separated AX_WRITE write scope into globs,
// dropping empty lines. An empty env yields a nil slice (the --no-write case).
func splitWriteGlobs(s string) []string {
	if s == "" {
		return nil
	}
	var globs []string
	for _, g := range strings.Split(s, "\n") {
		if g = strings.TrimSpace(g); g != "" {
			globs = append(globs, g)
		}
	}
	return globs
}

// Check runs the run's accept check (`ax check`) and prints its output and exit
// code, so a fenced session (which cannot run the check's mutating shell
// itself) can still verify completion the same way the outcome-tag accept check does. It
// execs $AX_ACCEPT and exits with the check's status.
func (a App) Check(args []string) {
	acc := os.Getenv("AX_ACCEPT")
	if acc == "" {
		fmt.Fprintln(os.Stderr, "ax: no accept check configured for this run (AX_ACCEPT unset)")
		os.Exit(2)
	}
	out, err := shell.Command(acc).CombinedOutput()
	os.Stdout.Write(out)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
}
