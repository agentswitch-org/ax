// Package retention applies archive-only session lifecycle policy.
package retention

import (
	"time"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
)

// ArchiveFilter selects which archive tier a view includes.
type ArchiveFilter int

const (
	ActiveOnly ArchiveFilter = iota
	All
	ArchivedOnly
)

func (f ArchiveFilter) Name() string {
	switch f {
	case All:
		return "all"
	case ArchivedOnly:
		return "archived"
	default:
		return "unarchived"
	}
}

// FilterSessions hides or selects archived sessions according to f.
func FilterSessions(sessions []session.Session, f ArchiveFilter) []session.Session {
	if f == All {
		return sessions
	}
	out := sessions[:0]
	for _, s := range sessions {
		switch f {
		case ArchivedOnly:
			if s.Archived {
				out = append(out, s)
			}
		default:
			if !s.Archived {
				out = append(out, s)
			}
		}
	}
	return out
}

// ApplyAuto archives safe, old, concluded ephemeral workers. The sessions slice
// is updated in place so the caller can filter without re-indexing.
func ApplyAuto(ret config.Retention, sessions []session.Session, rt map[string]state.Runtime, now time.Time) (int, error) {
	if !ret.AutoRetire {
		return 0, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	pending := ask.List()
	var n int
	var first error
	for i := range sessions {
		s := sessions[i]
		_, waiting := pending[s.ID]
		if !state.ShouldAutoRetire(state.AutoRetireInput{
			Parent:      s.Parent,
			Runtime:     rt[s.ID],
			Last:        s.Last,
			Now:         now,
			RetainAfter: ret.RetainDuration(),
			PendingAsk:  waiting,
			Archived:    s.Archived,
		}) {
			continue
		}
		if err := meta.SetArchived(s.ID, true); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		sessions[i].Archived = true
		sessions[i].ArchivedAt = time.Now()
		n++
	}
	return n, first
}

// Candidate is one session that a manual prune can archive.
type Candidate struct {
	Session   session.Session
	Lifecycle string
	Reason    string
}

// PruneCandidates returns local sessions safe for explicit archive pruning.
func PruneCandidates(ret config.Retention, sessions []session.Session, rt map[string]state.Runtime, run string, olderThan time.Duration, now time.Time) []Candidate {
	if now.IsZero() {
		now = time.Now()
	}
	pending := ask.List()
	var out []Candidate
	for _, s := range sessions {
		if s.Archived || s.Host != "" {
			continue
		}
		if run != "" && s.Group != run {
			continue
		}
		if !s.Last.IsZero() && olderThan > 0 && s.Last.After(now.Add(-olderThan)) {
			continue
		}
		if _, waiting := pending[s.ID]; !state.Ephemeral(s.Parent) || waiting {
			continue
		}
		r := rt[s.ID]
		if r.State == state.Live {
			continue
		}
		life := state.Lifecycle(r)
		switch {
		case life == state.LifecycleConcluded:
			out = append(out, Candidate{Session: s, Lifecycle: life, Reason: "concluded ephemeral"})
		case ret.PruneCrashed && life == state.LifecycleCrashed:
			out = append(out, Candidate{Session: s, Lifecycle: life, Reason: "crashed ghost"})
		}
	}
	return out
}
