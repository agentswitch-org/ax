package app

import (
	"strings"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
)

type adoptedWrapper struct {
	PID       int
	PGID      int
	LaunchID  string
	SessionID string
	Command   string
}

var scanAdoptedWrappersFn = platformScanAdoptedWrappers
var killAdoptedWrapperFn = platformKillAdoptedWrapper

func adoptedWorkerOrphans() map[string]adoptedWrapper {
	snap := live.Snapshot()
	out := map[string]adoptedWrapper{}
	for _, w := range scanAdoptedWrappersFn() {
		launchID := strings.TrimSpace(w.LaunchID)
		if launchID == "" {
			continue
		}
		id := resolveID(launchID)
		if id == "" {
			continue
		}
		if e, ok := snap[id]; ok && live.Running(e) {
			continue
		}
		w.LaunchID = launchID
		w.SessionID = id
		if _, ok := out[id]; !ok {
			out[id] = w
		}
	}
	return out
}

func adoptedWrapperRuntime(id string, sessions []session.Session, rt map[string]state.Runtime) state.Runtime {
	if r, ok := rt[id]; ok {
		r.State = state.Live
		if r.Activity == "" {
			r.Activity = state.Idle
			for _, s := range sessions {
				if s.ID == id && s.Host == "" {
					r.Activity = state.FileActivity(s)
					break
				}
			}
		}
		return r
	}
	r := state.Runtime{State: state.Live, Activity: state.Idle, Done: state.Done(id), Failed: state.Failed(id)}
	if at, ok := state.TerminalAt(id); ok {
		r.TerminalAt = at
	}
	return r
}

func syntheticAdoptedWorkerSession(id string, m meta.Meta) session.Session {
	last := m.Updated
	if at, ok := state.TerminalAt(id); ok {
		last = at
	}
	return session.Session{
		ID: id, Harness: m.Harness, Dir: m.Dir, Name: m.Name, Task: m.Task,
		Group: m.Group, Parent: m.Parent, Origin: m.Origin, Mode: m.Mode, Effort: m.Effort,
		Outcome: m.Outcome, FailReason: m.FailReason,
		Labels: m.Labels, HasSpec: m.Spec != nil, Archived: m.Archived, ArchivedAt: m.ArchivedAt,
		KeepLive: m.KeepLive, KeepUntil: m.KeepUntil,
		Last: last, Created: last,
	}
}

func pendingAsk(id string) bool {
	p, ok := ask.Load(id)
	return ok && !p.Answered
}

func adoptedWorkerOrphanSafe(id string, cfg config.Config, ret config.Retention) (adoptedWrapper, bool) {
	w, ok := adoptedWorkerOrphans()[id]
	if !ok {
		return adoptedWrapper{}, false
	}
	sessions := session.IndexReadOnly(cfg)
	if hooklessTurnStartedAfterTerminal(id, cfg, sessions) {
		return adoptedWrapper{}, false
	}
	rt := state.ComputeAll(sessions)
	r := adoptedWrapperRuntime(id, sessions, rt)
	if !workerReapSafeNow(id, meta.Load(id), ret, r, pendingAsk(id)) {
		return adoptedWrapper{}, false
	}
	return w, true
}

func adoptedWorkerOrphanKillSafe(id string, cfg config.Config) (adoptedWrapper, bool) {
	return adoptedWorkerOrphanSafe(id, cfg, config.Retention{ReapConcludedWorkers: true, ReapAfter: "0s"})
}

func (a App) killAdoptedWorkerOrphanIfSafe(id string, cfg config.Config) (bool, error) {
	w, ok := adoptedWorkerOrphanKillSafe(id, cfg)
	if !ok {
		return false, nil
	}
	return true, killAdoptedWrapperFn(w)
}
