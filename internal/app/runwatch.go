package app

import (
	"os"
	"strconv"
	"time"

	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/metrics"
	"github.com/agentswitch-org/ax/internal/models"
	"github.com/agentswitch-org/ax/internal/runs"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
	"github.com/agentswitch-org/ax/internal/view"
)

// fenceWatchInterval is how often a run's lifecycle owner polls the group totals.
// Cost/token fences do not need sub-second precision, so this stays cheap.
const fenceWatchInterval = 10 * time.Second

// RunOwner reports whether this session is a run's lifecycle owner: a root (no
// parent) of a group with at least one polled fence (cost/token/time). Only the
// owner polls, so there is one poller per run and still no daemon. The run wrapper
// checks this and, when true, spawns WatchFences.
func RunOwner() (group string, f fences, ok bool) {
	group = RunEnv()
	if group == "" || os.Getenv("AX_PARENT") != "" {
		return "", fences{}, false
	}
	f = envFences()
	ok = f.maxCost > 0 || f.maxTokens > 0 || f.timeout > 0
	return
}

// RunEnv reads the current session's run id from AX_RUN, falling back to the
// deprecated AX_GROUP for compat with anything that still sets (or only reads)
// the old name.
func RunEnv() string {
	if v := os.Getenv("AX_RUN"); v != "" {
		return v
	}
	return os.Getenv("AX_GROUP")
}

// envFences reads a run's fences from the injected AX_MAX_* environment.
func envFences() fences {
	var f fences
	f.maxCost = envFloat("AX_MAX_COST")
	f.maxTokens = envFloat("AX_MAX_TOKENS")
	f.maxWorkers = envInt("AX_MAX_WORKERS", 0)
	f.maxDepth = envInt("AX_MAX_DEPTH", 0)
	if d := os.Getenv("AX_TIMEOUT"); d != "" {
		f.timeout, _ = time.ParseDuration(d)
	}
	return f
}

// WatchFences polls a group's cost/token/time totals on a cadence and, when a
// fence trips, writes the run record (outcome budget_hit) and cascade-kills the
// run. It runs inside the root's run wrapper; killing the group ends the root,
// so this returns. This is the "cap the downside" mechanism: pure poll-and-kill.
func (a App) WatchFences(group string, f fences, start time.Time) {
	cfg, _ := config.Load()
	db := models.Load()
	t := time.NewTicker(fenceWatchInterval)
	defer t.Stop()
	for range t.C {
		var cost float64
		var toks int
		for _, s := range filterGroup(session.Index(cfg), group) {
			if c := view.Cost(s, db); c > 0 {
				cost += c
			}
			toks += s.InTok + s.OutTok + s.CacheReadT + s.CacheWriteT
		}
		if (f.maxCost > 0 && cost >= f.maxCost) ||
			(f.maxTokens > 0 && float64(toks) >= f.maxTokens) ||
			(f.timeout > 0 && time.Since(start) >= f.timeout) {
			a.writeRun(cfg, db, group, runs.BudgetHit)
			a.cascadeKill(cfg, group)
			return
		}
	}
}

// ConcludeRun writes the run record when a root exits, unless a fence already
// wrote one. The outcome is the root's tagged outcome, else gave_up (idle is
// never success).
func (a App) ConcludeRun(group string) {
	if group == "" || runs.Exists(group) {
		return
	}
	// A restart tears the root down and immediately relaunches it; its dying
	// wrapper lands here, but the run is being reopened, not given up. Swallow this
	// one conclusion (clearing the one-shot marker) so the relaunched session
	// concludes normally later.
	if runs.Suppressed(group) {
		runs.Unsuppress(group)
		return
	}
	cfg, _ := config.Load()
	db := models.Load()
	outcome := runs.GaveUp
	for _, s := range filterGroup(session.Index(cfg), group) {
		if s.Parent == "" && s.Outcome != "" {
			outcome = s.Outcome
		}
	}
	a.writeRun(cfg, db, group, outcome)
}

// writeRun snapshots the group and saves its run record.
func (a App) writeRun(cfg config.Config, db models.DB, group, outcome string) {
	sessions := filterGroup(session.Index(cfg), group)
	loc := a.live()
	rt := state.ComputeAll(sessions)
	liveState := func(id string) string {
		if _, ok := loc[id]; ok {
			return state.Live
		}
		return rt[id].State
	}
	runs.Save(runs.Build(group, outcome, sessions,
		func(s session.Session) float64 { return view.Cost(s, db) }, liveState))
	// Same run-conclusion choke point D15 uses for the run-success notify hook:
	// every conclusion path (clean exit, fence trip) funnels through writeRun, so
	// this is the one place a config-gated side effect belongs, not a second poller.
	if cfg.Metrics.Textfile != "" {
		if err := metrics.WriteTextfile(cfg.Metrics.Textfile, metrics.Build(cfg, db)); err != nil {
			axlog.Printf("metrics: %v", err)
		}
	}
}

func envFloat(k string) float64 {
	if v := os.Getenv(k); v != "" {
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return 0
}
