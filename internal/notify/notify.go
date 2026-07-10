// Package notify reaches a human when a run hits a lifecycle event without a
// daemon: it fires a user-configured command from the same ax verb that creates
// the transition. Two built-ins (bell, tmux) cover the common cases; anything
// richer rides the hook as a shell command.
//
// Three event groups are fireable today:
//
//   - needs-you / done-review (attention states): fired from `ax ask` and
//     `ax hookstate blocked`.
//   - run-success / run-fail (run outcomes): fired from the done-gate in
//     `ax tag --outcome`.
//
// Crash and exit are NOT fireable from config (they need a watcher; that is
// outside the no-daemon boundary).
package notify

import (
	"os"
	"os/exec"
	"strings"

	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/shell"
)

// Lifecycle event names. Attention events fire from ax ask and ax hookstate;
// run events fire from the done-gate in ax tag --outcome.
const (
	NeedsYou   = "needs-you"
	DoneReview = "done-review"
	RunSuccess = "run-success"
	RunFail    = "run-fail"
)

// Config is the parsed notify configuration. It handles two TOML forms:
//
//	notify = "bell"             # string shorthand, fires on attention states
//	[notify]                    # table form, maps event name -> command template
//	run-success = "ax send ..."
//
// The string shorthand fires for needs-you and done-review only (back-compat).
// The table form fires for any named event whose key appears in the table.
// The two forms are mutually exclusive in one TOML file (the key is either a
// string or a table, not both); use the table form when you need run-success.
type Config struct {
	// Attention is the plain-string shorthand. Fires on needs-you and done-review.
	Attention string
	// Events maps lifecycle event names to command templates. Keys are the event
	// constants above (e.g. "run-success", "needs-you"). A missing key is a no-op
	// for that event.
	Events map[string]string
}

// UnmarshalTOML implements toml.Unmarshaler so notify = "bell" (string) and
// [notify] (table) are both valid TOML forms for the same key.
func (c *Config) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		c.Attention = x
	case map[string]any:
		c.Events = make(map[string]string)
		for k, ev := range x {
			if s, ok := ev.(string); ok {
				c.Events[k] = s
			}
		}
	}
	return nil
}

// Event is one lifecycle transition: which session, which event, and the
// context a human wants to read (the question or task, the name, the run).
type Event struct {
	ID      string
	State   string // event name: NeedsYou | DoneReview | RunSuccess | RunFail
	Summary string // the ask question, or the session's task
	Name    string
	Group   string
}

// Fire dispatches a lifecycle event. Resolution order:
//  1. If cfg.Events is set, look up ev.State in the table.
//  2. For attention states (needs-you, done-review), fall back to cfg.Attention
//     (the plain-string shorthand, for back-compat with notify = "bell").
//
// It never blocks or returns an error; failures are logged to ax log.
func Fire(cfg Config, ev Event) {
	cmd := ""
	if cfg.Events != nil {
		cmd = cfg.Events[ev.State]
	}
	// Fall back to the string shorthand for attention states only. The string
	// form was defined as an attention-state hook, so it must not fire for run
	// events (run-success, run-fail) to keep its semantics stable.
	if cmd == "" && (ev.State == NeedsYou || ev.State == DoneReview) {
		cmd = cfg.Attention
	}

	switch strings.TrimSpace(cmd) {
	case "":
		return
	case "bell":
		// A BEL on stderr: `ax ask` runs in the session's own tmux window, so tmux
		// flags that window (bell/activity), the native "this window wants you".
		os.Stderr.Write([]byte{'\a'})
		return
	case "tmux":
		run(true, "tmux", "display-message", "ax: "+ev.State+" "+ev.Summary)
		return
	}
	// A user command: fill the template with shell-quoted values (the summary is
	// agent-authored text, never interpolated raw) and fire it detached, so a slow
	// webhook cannot delay the agent or the blocking ask. {state} and {event} both
	// carry the event name so existing templates keep working and new ones can use
	// the clearer {event} name. Likewise {group} and {run} both carry the run id;
	// {run} is the current name (see the group->run rename), {group} a deprecated
	// alias kept for existing templates.
	filled := strings.NewReplacer(
		"{id}", quote(ev.ID),
		"{state}", quote(ev.State),
		"{event}", quote(ev.State),
		"{summary}", quote(ev.Summary),
		"{name}", quote(ev.Name),
		"{run}", quote(ev.Group),
		"{group}", quote(ev.Group),
	).Replace(cmd)
	runCmd(false, shell.Background(filled))
}

// run execs a notify command. wait=true blocks for it (the fast built-ins);
// wait=false fires it detached via a backgrounding shell that exits at once, so
// a slow user command never holds the caller. Errors are logged, never returned.
func run(wait bool, name string, args ...string) {
	runCmd(wait, exec.Command(name, args...))
}

// runCmd starts c and, when wait is false, fires it detached: the backgrounding
// shell exits immediately and the real command is reparented, so a slow user
// command never holds the caller. Errors are logged, never returned.
func runCmd(wait bool, c *exec.Cmd) {
	if err := c.Start(); err != nil {
		axlog.Printf("notify: %v", err)
		return
	}
	if wait {
		c.Wait()
		return
	}
	go c.Wait()
}

// quote wraps a value for safe substitution into a shell command, delegating to
// the platform shell layer (POSIX single quotes on unix, PowerShell quoting on
// Windows), the same technique the launch templates use for untrusted values.
func quote(s string) string { return shell.Quote(s) }
