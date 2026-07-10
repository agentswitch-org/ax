// Package ask is the human ask/reply channel: a session calls `ax ask`, which
// writes a pending-question sidecar and blocks polling it; a human answers with
// `ax reply` (or a picker action), which writes the answer back and unblocks the
// call. File-based, one file per asking session at $XDG_STATE_HOME/ax/ask/<id>.json,
// so it matches ax's no-daemon style and the picker can see who is waiting.
package ask

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
)

// Pending is one outstanding question and its (eventual) answer.
type Pending struct {
	Question string    `json:"question"`
	Answer   string    `json:"answer,omitempty"`
	Answered bool      `json:"answered"`
	Asked    time.Time `json:"asked"`
}

func dir() string { return axdir.State("ask") }

func path(id string) string { return filepath.Join(dir(), id+".json") }

func readDir() string { return axdir.StatePath("ask") }

func readPath(id string) string { return filepath.Join(readDir(), id+".json") }

// Load returns a session's pending question, if any.
func Load(id string) (Pending, bool) {
	var p Pending
	data, err := os.ReadFile(readPath(id))
	if err != nil {
		return p, false
	}
	if json.Unmarshal(data, &p) != nil {
		return p, false
	}
	return p, true
}

// Save writes a pending question atomically, so the blocked `ax ask` poll on
// the other side never reads a torn file and mistakes the answer for a cancel.
func Save(id string, p Pending) error {
	return axdir.WriteJSON(path(id), p)
}

// Answer records a reply, unblocking the waiting `ax ask` on its next poll.
func Answer(id, answer string) error {
	p, ok := Load(id)
	if !ok {
		return os.ErrNotExist
	}
	p.Answer = answer
	p.Answered = true
	return Save(id, p)
}

// Remove clears a session's pending question (on answer, or teardown).
func Remove(id string) { os.Remove(readPath(id)) }

// List returns every session id currently waiting on a human, so the picker can
// surface "needs you".
func List() map[string]Pending {
	out := map[string]Pending{}
	es, err := os.ReadDir(readDir())
	if err != nil {
		return out
	}
	for _, e := range es {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if p, ok := Load(id); ok && !p.Answered {
			out[id] = p
		}
	}
	return out
}
