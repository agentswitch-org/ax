package app

import "strings"

// axPreamble is the fixed system-prompt text every ax launch carries ahead of
// any user behavior. It is the one fact that makes the control layer usable
// from inside a session: the model learns that ax exists, what it does, and
// to read `ax help` before driving it. Without it a session launched by
// `ax claude "TASK"` sees AX_SESSION_ID in its environment and nothing else.
func axPreamble(id, run string) string {
	var b strings.Builder
	b.WriteString("# ax\n\n")
	b.WriteString("You run under ax, a CLI that launches, reads, waits on, messages, and restarts coding-agent sessions.\n")
	if id != "" {
		b.WriteString("Your ax session id is " + id + ".")
		if run != "" {
			b.WriteString(" Your run is " + run + ".")
		}
		b.WriteString("\n")
	}
	b.WriteString("Before you use ax, run `ax help` once and follow it exactly.\n\n")
	b.WriteString("Common verbs:\n")
	b.WriteString("- delegate: `ax claude --task-file PATH` or `ax codex --task-file PATH` (prints the new session id)\n")
	b.WriteString("- follow up: `ax wait ID`, `ax result ID`, `ax read ID`\n")
	b.WriteString("- recover: `ax search \"TEXT\" --json`, `ax list --run RUN --json`, `ax restart ID`, `ax continue ID \"TASK\"`\n")
	b.WriteString("- talk: `ax send ID \"TEXT\"` types into a live session; `ax ask \"QUESTION\"` blocks for a human answer\n")
	return b.String()
}

// withPreamble prepends the ax preamble to a resolved behavior text. An empty
// behavior yields the preamble alone.
func withPreamble(id, run, behavior string) string {
	p := axPreamble(id, run)
	if strings.TrimSpace(behavior) == "" {
		return p
	}
	return p + "\n" + behavior
}
