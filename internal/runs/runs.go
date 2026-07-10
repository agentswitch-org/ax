// Package runs stores a run record per group at $XDG_STATE_HOME/ax/runs/<gid>.json,
// written once when a run concludes (the root tagged an outcome, a fence tripped,
// or the root exited). It is ax's post-run telemetry: the run tree plus cost
// and token totals, all ax-measured fact, stamped with the root-reported outcome.
// Topology + cost -> outcome is labeled data for tuning an orchestrating agent's behavior.
package runs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/session"
)

// Outcomes a run concludes with.
const (
	Success   = "success"
	GaveUp    = "gave_up"
	BudgetHit = "budget_hit"
	Crashed   = "crashed"
)

// Tokens are a run's summed token usage.
type Tokens struct {
	In         int `json:"in"`
	Out        int `json:"out"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

// Node is one session in the run's tree.
type Node struct {
	ID      string  `json:"id"`
	Parent  string  `json:"parent,omitempty"`
	Name    string  `json:"name,omitempty"`
	Task    string  `json:"task,omitempty"`
	Cost    float64 `json:"cost"`
	Turns   int     `json:"turns"`
	State   string  `json:"state,omitempty"`
	Outcome string  `json:"outcome,omitempty"`
}

// Record is one run from launch to conclusion.
type Record struct {
	Group     string    `json:"group"`
	Root      string    `json:"root"`
	Task      string    `json:"task"`
	Cost      float64   `json:"cost"`
	Tokens    Tokens    `json:"tokens"`
	Workers   int       `json:"workers"`
	MaxDepth  int       `json:"max_depth"`
	Tree      []Node    `json:"tree"`
	Outcome   string    `json:"outcome"`
	Started   time.Time `json:"started,omitempty"` // root's first transcript timestamp; zero on records written before this field existed
	Concluded time.Time `json:"concluded"`
}

func dir() string { return axdir.State("runs") }

func path(group string) string { return filepath.Join(dir(), group+".json") }

// Exists reports whether a run record was already written for the group, so a
// clean-exit conclusion does not overwrite a fence-trip record.
func Exists(group string) bool {
	_, err := os.Stat(path(group))
	return err == nil
}

// Remove deletes a group's run record. A fresh root launch reusing a group name
// calls this, so the new run's conclusion (and a --wait exit code) never reads
// the previous run's record.
func Remove(group string) { os.Remove(path(group)) }

func suppressPath(group string) string { return filepath.Join(dir(), group+".suppress") }

// Suppress marks a group so the next conclusion is skipped. `ax restart` sets it
// before it kills a session, so the dying root's run wrapper does not record the
// run as gave_up when restart is actually reopening it. Cleared by the first
// ConcludeRun that sees it (the dying wrapper).
func Suppress(group string) { axdir.WriteFileAtomic(suppressPath(group), []byte("1"), 0o600) }

// Suppressed reports whether a group's next conclusion should be skipped.
func Suppressed(group string) bool {
	_, err := os.Stat(suppressPath(group))
	return err == nil
}

// Unsuppress clears a group's suppression marker (a one-shot: the swallowed
// conclusion clears it so the relaunched session concludes normally).
func Unsuppress(group string) { os.Remove(suppressPath(group)) }

// Build snapshots a group into a Record, stamping outcome. cost yields a session's
// dollar cost (the caller supplies it, keeping this package free of pricing).
func Build(group, outcome string, sessions []session.Session, cost func(session.Session) float64, live func(id string) string) Record {
	r := Record{Group: group, Outcome: outcome, Concluded: time.Now()}
	depth := map[string]int{}
	byID := map[string]session.Session{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	for _, s := range sessions {
		if s.Parent == "" {
			r.Root, r.Task, r.Started = s.ID, s.Task, s.Created
		}
		c := cost(s)
		if c > 0 {
			r.Cost += c
		}
		r.Tokens.In += s.InTok
		r.Tokens.Out += s.OutTok
		r.Tokens.CacheRead += s.CacheReadT
		r.Tokens.CacheWrite += s.CacheWriteT
		r.Workers++
		d := treeDepth(s, byID)
		depth[s.ID] = d
		if d > r.MaxDepth {
			r.MaxDepth = d
		}
		r.Tree = append(r.Tree, Node{
			ID: s.ID, Parent: s.Parent, Name: s.Name, Task: s.Task,
			Cost: c, State: live(s.ID), Outcome: s.Outcome,
		})
	}
	sort.SliceStable(r.Tree, func(i, j int) bool { return depth[r.Tree[i].ID] < depth[r.Tree[j].ID] })
	return r
}

func treeDepth(s session.Session, byID map[string]session.Session) int {
	d, seen := 0, map[string]bool{}
	for s.Parent != "" && !seen[s.ID] {
		seen[s.ID] = true
		p, ok := byID[s.Parent]
		if !ok {
			break
		}
		d++
		s = p
	}
	return d
}

// Load returns a group's run record, if one was written.
func Load(group string) (Record, bool) {
	data, err := os.ReadFile(path(group))
	if err != nil {
		return Record{}, false
	}
	var r Record
	if json.Unmarshal(data, &r) != nil {
		return Record{}, false
	}
	return r, true
}

// Save writes a run record atomically.
func Save(r Record) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return axdir.WriteFileAtomic(path(r.Group), data, 0o600)
}

// List returns every run record, newest first.
func List() []Record {
	es, err := os.ReadDir(dir())
	if err != nil {
		return nil
	}
	var out []Record
	for _, e := range es {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if data, err := os.ReadFile(filepath.Join(dir(), e.Name())); err == nil {
			var r Record
			if json.Unmarshal(data, &r) == nil {
				out = append(out, r)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Concluded.After(out[j].Concluded) })
	return out
}
