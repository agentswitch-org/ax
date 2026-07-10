package app

import (
	"strings"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
)

// ReopenIfTurnStartedAfterTerminal clears a stale terminal outcome when a
// hookless harness records a newer turn start in the same session. This mirrors
// continueLiveReuse's source-state reset for the same-TUI input path:
// wait/result must track the reopened turn, not a prior turn_aborted or done
// marker.
func ReopenIfTurnStartedAfterTerminal(id, format, file string) bool {
	terminalAt, ok := state.TerminalAt(id)
	if !ok || !session.TurnStartedAfter(format, file, terminalAt) {
		return false
	}
	ReopenTurnLifecycle(id)
	return true
}

// ReopenTurnLifecycle clears terminal result state while preserving session
// identity, task, grouping, labels, and keep-live metadata.
func ReopenTurnLifecycle(id string) {
	meta.Update(id, func(m *meta.Meta) {
		m.Outcome = ""
		m.FailReason = ""
		m.Result = ""
		m.Exit = nil
	})
	state.RemoveHook(id)
}

// refreshReopenedTurn is a defensive CLI-side refresh for result/wait. The run
// wrapper watcher also calls ReopenIfTurnStartedAfterTerminal, but a separate
// process can ask for result/wait in the brief window before the watcher polls.
func refreshReopenedTurn(id string) bool {
	if id == "" || !state.Terminal(id) {
		return false
	}
	e, ok := live.Snapshot()[id]
	if !ok || !live.Running(e) {
		return false
	}
	cfg, _ := config.Load()
	return refreshReopenedTurnFromIndex(id, cfg)
}

// refreshReopenedTurnAtExit is the run wrapper's last chance to repair a stale
// terminal marker before ConcludeExit records this process's real outcome. It
// does not require a live heartbeat because the caller is the wrapper that is
// exiting now.
func refreshReopenedTurnAtExit(id string) bool {
	if id == "" || !state.Terminal(id) {
		return false
	}
	cfg, _ := config.Load()
	return refreshReopenedTurnFromIndex(id, cfg)
}

func refreshReopenedTurnFromIndex(id string, cfg config.Config) bool {
	m := meta.Load(id)
	if !reopenableHooklessMode(m.Mode) || strings.TrimSpace(m.Task) == "" {
		return false
	}
	formats := harnessFormats(cfg)
	format := formats[m.Harness]
	for _, s := range session.Index(cfg) {
		if s.ID == id && s.Host == "" {
			if format == "" {
				format = formats[s.Harness]
			}
			if format != "pi" && format != "codex" {
				// The same-session reopen path only applies to hookless harnesses.
				// Claude's own lifecycle hooks are authoritative.
				return false
			}
			return ReopenIfTurnStartedAfterTerminal(id, format, s.File)
		}
	}
	return false
}

// refreshReopenedTurns refreshes a precomputed local index before list/report
// state is derived from it. The caller should re-index when it returns true so
// cleared outcome fields are reflected in the rows it renders.
func refreshReopenedTurns(cfg config.Config, sessions []session.Session) bool {
	if len(sessions) == 0 {
		return false
	}
	snap := live.Snapshot()
	formats := harnessFormats(cfg)
	changed := false
	for _, s := range sessions {
		if s.Host != "" || s.ID == "" || !state.Terminal(s.ID) {
			continue
		}
		e, ok := snap[s.ID]
		if !ok || !live.Running(e) {
			continue
		}
		if !reopenableHooklessMode(s.Mode) || strings.TrimSpace(s.Task) == "" {
			continue
		}
		format := formats[s.Harness]
		if format != "pi" && format != "codex" {
			continue
		}
		if ReopenIfTurnStartedAfterTerminal(s.ID, format, s.File) {
			changed = true
		}
	}
	return changed
}

func reopenableHooklessMode(mode string) bool {
	return mode == "interactive" || mode == "headless"
}
