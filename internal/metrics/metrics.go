// Package metrics aggregates ax's existing session and run telemetry
// (internal/session, internal/runs) into one snapshot for `ax metrics`: a
// human table, --json for scripting, and --prom for node_exporter's textfile
// collector. It computes nothing new, only reshapes what ax already tracks.
package metrics

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/models"
	"github.com/agentswitch-org/ax/internal/runs"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

// SessionMetric is one indexed session's cost/token/duration facts. HasCost is
// false when the session's model has no price-DB entry (view.Cost's -1
// sentinel), so a caller doesn't mistake "no estimate possible" for "free".
type SessionMetric struct {
	ID         string        `json:"id"`
	Harness    string        `json:"harness"`
	Model      string        `json:"model"`
	Cost       float64       `json:"cost"`
	HasCost    bool          `json:"has_cost"`
	InTok      int           `json:"in_tokens"`
	OutTok     int           `json:"out_tokens"`
	CacheRead  int           `json:"cache_read_tokens"`
	CacheWrite int           `json:"cache_write_tokens"`
	Duration   time.Duration `json:"duration_ns"`
	Outcome    string        `json:"outcome,omitempty"`
}

// RunMetric is one concluded run's cost/token/duration facts.
type RunMetric struct {
	Group    string        `json:"group"`
	Outcome  string        `json:"outcome"`
	Cost     float64       `json:"cost"`
	Tokens   runs.Tokens   `json:"tokens"`
	Workers  int           `json:"workers"`
	MaxDepth int           `json:"max_depth"`
	Duration time.Duration `json:"duration_ns"`
	Task     string        `json:"task"`
}

// Snapshot is every session and concluded run ax knows about, as of Build.
type Snapshot struct {
	Sessions []SessionMetric `json:"sessions"`
	Runs     []RunMetric     `json:"runs"`
}

// Build reads session.Index and runs.List (the same sources `ax list` and
// `ax runs` read) into one snapshot.
func Build(cfg config.Config, db models.DB) Snapshot {
	var snap Snapshot
	for _, s := range session.Index(cfg) {
		cost := view.Cost(s, db)
		hasCost := cost >= 0
		if !hasCost {
			cost = 0
		}
		snap.Sessions = append(snap.Sessions, SessionMetric{
			ID: s.ID, Harness: s.Harness, Model: s.Model,
			Cost:       cost,
			HasCost:    hasCost,
			InTok:      s.InTok,
			OutTok:     s.OutTok,
			CacheRead:  s.CacheReadT,
			CacheWrite: s.CacheWriteT,
			Duration:   duration(s.Created, s.Last),
			Outcome:    s.Outcome,
		})
	}
	for _, r := range runs.List() {
		snap.Runs = append(snap.Runs, RunMetric{
			Group: r.Group, Outcome: r.Outcome, Cost: r.Cost, Tokens: r.Tokens,
			Workers: r.Workers, MaxDepth: r.MaxDepth,
			Duration: duration(r.Started, r.Concluded), Task: r.Task,
		})
	}
	return snap
}

func duration(start, end time.Time) time.Duration {
	if start.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start)
}

// tokenKinds is the fixed, ordered set of token kinds every rendering walks,
// so table columns and prom series come out in the same order every time.
var tokenKinds = []string{"in", "out", "cache_read", "cache_write"}

func tokenVals(in, out, cacheRead, cacheWrite int) map[string]int {
	return map[string]int{"in": in, "out": out, "cache_read": cacheRead, "cache_write": cacheWrite}
}

// RenderTable renders the human-readable form: a sessions table, a runs
// table, and an outcome-count summary line.
func RenderTable(snap Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SESSIONS  %d\n", len(snap.Sessions))
	fmt.Fprintf(&b, "%-38s %-9s %-20s %8s %8s %8s %8s %8s\n",
		"ID", "HARNESS", "MODEL", "COST", "IN", "OUT", "CACHE", "DUR")
	for _, s := range snap.Sessions {
		fmt.Fprintf(&b, "%-38s %-9s %-20s %8s %8s %8s %8s %8s\n",
			s.ID, s.Harness, view.Clip(s.Model, 20), costCell(s.Cost, s.HasCost),
			view.HumanTok(s.InTok), view.HumanTok(s.OutTok),
			view.HumanTok(s.CacheRead+s.CacheWrite), humanDur(s.Duration))
	}

	fmt.Fprintf(&b, "\nRUNS  %d\n", len(snap.Runs))
	fmt.Fprintf(&b, "%-14s %-10s %8s %8s %8s %8s %4s %8s\n",
		"RUN", "OUTCOME", "COST", "IN", "OUT", "CACHE", "WKRS", "DUR")
	for _, r := range snap.Runs {
		fmt.Fprintf(&b, "%-14s %-10s %8s %8s %8s %8s %4d %8s\n",
			r.Group, r.Outcome, view.CostShort(r.Cost),
			view.HumanTok(r.Tokens.In), view.HumanTok(r.Tokens.Out),
			view.HumanTok(r.Tokens.CacheRead+r.Tokens.CacheWrite), r.Workers, humanDur(r.Duration))
	}

	counts := map[string]int{}
	for _, r := range snap.Runs {
		counts[r.Outcome]++
	}
	fmt.Fprint(&b, "\nOUTCOMES  ")
	parts := make([]string, 0, len(counts))
	for _, o := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s=%d", o, counts[o]))
	}
	fmt.Fprintln(&b, strings.Join(parts, " "))
	return b.String()
}

// costCell matches the picker's no-data convention: a session whose model has
// no price-DB entry shows "-", never a fabricated $0.
func costCell(cost float64, hasCost bool) string {
	if !hasCost {
		return "-"
	}
	return view.CostShort(cost)
}

func humanDur(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// RenderProm renders the Prometheus textfile-collector form. Series are
// aggregated (totals and per-outcome sums), never keyed by session id or run
// group, so cardinality stays bounded no matter how many sessions or runs ax
// has ever recorded.
func RenderProm(snap Snapshot) string {
	var b strings.Builder

	var sessCost float64
	var sessIn, sessOut, sessCR, sessCW int
	for _, s := range snap.Sessions {
		if s.HasCost {
			sessCost += s.Cost
		}
		sessIn += s.InTok
		sessOut += s.OutTok
		sessCR += s.CacheRead
		sessCW += s.CacheWrite
	}
	fmt.Fprintln(&b, "# HELP ax_sessions_indexed Sessions ax has indexed across every configured harness.")
	fmt.Fprintln(&b, "# TYPE ax_sessions_indexed gauge")
	fmt.Fprintf(&b, "ax_sessions_indexed %d\n", len(snap.Sessions))
	fmt.Fprintln(&b, "# HELP ax_session_cost_dollars Summed cost of every indexed session.")
	fmt.Fprintln(&b, "# TYPE ax_session_cost_dollars gauge")
	fmt.Fprintf(&b, "ax_session_cost_dollars %g\n", sessCost)
	fmt.Fprintln(&b, "# HELP ax_session_tokens Summed session token usage by kind.")
	fmt.Fprintln(&b, "# TYPE ax_session_tokens gauge")
	sessTok := tokenVals(sessIn, sessOut, sessCR, sessCW)
	for _, k := range tokenKinds {
		fmt.Fprintf(&b, "ax_session_tokens{kind=%q} %d\n", k, sessTok[k])
	}

	type runAgg struct {
		count              int
		cost               float64
		in, out, cr, cw    int
		durationSecondsSum float64
	}
	byOutcome := map[string]*runAgg{}
	for _, r := range snap.Runs {
		a := byOutcome[r.Outcome]
		if a == nil {
			a = &runAgg{}
			byOutcome[r.Outcome] = a
		}
		a.count++
		a.cost += r.Cost
		a.in += r.Tokens.In
		a.out += r.Tokens.Out
		a.cr += r.Tokens.CacheRead
		a.cw += r.Tokens.CacheWrite
		a.durationSecondsSum += r.Duration.Seconds()
	}
	outcomes := make([]string, 0, len(byOutcome))
	for o := range byOutcome {
		outcomes = append(outcomes, o)
	}
	sort.Strings(outcomes)

	fmt.Fprintln(&b, "# HELP ax_runs_concluded Concluded runs by outcome.")
	fmt.Fprintln(&b, "# TYPE ax_runs_concluded gauge")
	for _, o := range outcomes {
		fmt.Fprintf(&b, "ax_runs_concluded{outcome=%q} %d\n", o, byOutcome[o].count)
	}
	fmt.Fprintln(&b, "# HELP ax_run_cost_dollars Summed cost of concluded runs by outcome.")
	fmt.Fprintln(&b, "# TYPE ax_run_cost_dollars gauge")
	for _, o := range outcomes {
		fmt.Fprintf(&b, "ax_run_cost_dollars{outcome=%q} %g\n", o, byOutcome[o].cost)
	}
	fmt.Fprintln(&b, "# HELP ax_run_tokens Summed run token usage by outcome and kind.")
	fmt.Fprintln(&b, "# TYPE ax_run_tokens gauge")
	for _, o := range outcomes {
		a := byOutcome[o]
		runTok := tokenVals(a.in, a.out, a.cr, a.cw)
		for _, k := range tokenKinds {
			fmt.Fprintf(&b, "ax_run_tokens{outcome=%q,kind=%q} %d\n", o, k, runTok[k])
		}
	}
	fmt.Fprintln(&b, "# HELP ax_run_duration_seconds Wall-clock duration of concluded runs by outcome (sum/count, so avg = sum/count in PromQL).")
	fmt.Fprintln(&b, "# TYPE ax_run_duration_seconds gauge")
	for _, o := range outcomes {
		fmt.Fprintf(&b, "ax_run_duration_seconds_sum{outcome=%q} %g\n", o, byOutcome[o].durationSecondsSum)
		fmt.Fprintf(&b, "ax_run_duration_seconds_count{outcome=%q} %d\n", o, byOutcome[o].count)
	}
	return b.String()
}

// WriteTextfile renders snap as Prometheus text and writes it atomically to
// path, creating its directory if needed (node_exporter's textfile collector
// expects a plain file, no daemon holding it open).
func WriteTextfile(path string, snap Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return axdir.WriteFileAtomic(path, []byte(RenderProm(snap)), 0o644)
}
