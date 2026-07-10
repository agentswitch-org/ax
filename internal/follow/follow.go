// Package follow streams turn-boundary events for a session (or a whole group)
// without a daemon: it runs its own poll loop, diffing the live heartbeat and the
// transcript every few hundred milliseconds and emitting one event per boundary.
// The loop lives in the process that reads it (`ax read --follow`) and dies with
// it, so nothing is left running. A turn boundary is quiescence after output: the
// transcript gained records, then the session went idle.
package follow

import (
	"context"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/session"
)

const poll = 350 * time.Millisecond

// Event is one NDJSON line of the stream.
type Event struct {
	Host    string `json:"host,omitempty"`
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Group   string `json:"group,omitempty"`
	Event   string `json:"event"` // turn | waiting | exit | crash | output
	Cursor  int    `json:"cursor,omitempty"`
	Role    string `json:"role,omitempty"`
	Ts      string `json:"ts,omitempty"`
	Preview string `json:"preview,omitempty"`
	Reason  string `json:"reason,omitempty"` // input | auth (waiting)
	Hint    string `json:"hint,omitempty"`
}

// emit sends an event unless the stream was cancelled, so a sender never blocks
// forever on an abandoned channel (and never races a close after cancellation).
func emit(ctx context.Context, out chan<- Event, e Event) bool {
	select {
	case out <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

// Target is one session to follow.
type Target struct {
	ID, Name, Group, Format, File, WaitingRe string
	StartCursor                              int
	UseStartCursor                           bool
}

// Options tune the stream.
type Options struct {
	Since       int             // start cursor (0 = from the beginning)
	WithContent bool            // inline full turn text instead of a short preview
	Events      map[string]bool // which event types to emit; nil = turn/waiting/exit/crash
	PaneTail    func(id string) string
	// Snap overrides the heartbeat snapshot source. Group installs a shared
	// memoized one so N member streams read the live dir once per poll, not N
	// times.
	Snap func() map[string]live.Entry
}

func (o Options) snapshot() map[string]live.Entry {
	if o.Snap != nil {
		return o.Snap()
	}
	return live.Snapshot()
}

func (o Options) wants(ev string) bool {
	if o.Events == nil {
		return ev != "output" // output is opt-in
	}
	return o.Events[ev]
}

func startCursor(t Target, o Options) int {
	if t.UseStartCursor {
		return t.StartCursor
	}
	return o.Since
}

var authRe = regexp.MustCompile(`(?i)https?://\S+|oauth|sign in|log ?in|authenticate|paste the code`)

// Stream follows one target, emitting events on out until ctx is cancelled or the
// session ends (exit/crash). Safe to run in a goroutine.
func Stream(ctx context.Context, t Target, o Options, out chan<- Event) {
	cursor := startCursor(t, o)
	var pending []session.NormTurn
	var lastSize int64
	var lastMod time.Time
	beat := false // a heartbeat was seen at least once
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		e, ok := o.snapshot()[t.ID]
		switch {
		case ok && !live.Running(e): // stale heartbeat or dead wrapper pid: a crash
			if o.wants("crash") {
				emit(ctx, out, Event{ID: t.ID, Name: t.Name, Group: t.Group, Event: "crash", Cursor: cursor})
			}
			return
		case ok:
			beat = true
		case beat: // the heartbeat it had is gone: a clean exit
			if o.wants("exit") {
				emit(ctx, out, Event{ID: t.ID, Name: t.Name, Group: t.Group, Event: "exit", Cursor: cursor})
			}
			return
			// Never had a beat: a just-launched worker whose wrapper hasn't started,
			// or a hand-started (adopted) session with no wrapper at all. Keep
			// following the transcript; the caller's ctx bounds the stream.
		}

		// Reparse only when the transcript changed (cheap stat gate).
		if st, err := os.Stat(t.File); t.File != "" && err == nil {
			if st.Size() != lastSize || !st.ModTime().Equal(lastMod) {
				lastSize, lastMod = st.Size(), st.ModTime()
				fresh, cur := session.Turns(t.Format, t.File, cursor)
				if len(fresh) > 0 {
					pending = append(pending, fresh...)
					cursor = cur
					if o.wants("output") {
						emit(ctx, out, Event{ID: t.ID, Name: t.Name, Group: t.Group, Event: "output", Cursor: cursor,
							Preview: preview(fresh, o.WithContent)})
					}
				}
			}
		}

		// A turn boundary is pending records plus quiescence (idle for state.Active).
		if len(pending) > 0 && time.Since(lastMod) >= live.Active {
			last := pending[len(pending)-1]
			ev := Event{ID: t.ID, Name: t.Name, Group: t.Group, Event: "turn", Cursor: cursor,
				Role: last.Role, Ts: last.Ts, Preview: preview(pending, o.WithContent)}
			if reason, hint := waiting(t, o); reason != "" {
				ev.Event, ev.Reason, ev.Hint = "waiting", reason, hint
			}
			if o.wants(ev.Event) {
				emit(ctx, out, ev)
			}
			pending = nil
		}
	}
}

// Group follows every member the members() snapshot returns, re-evaluating
// membership each interval so a worker spawned after the follow began joins the
// stream. It runs until ctx is cancelled.
func Group(ctx context.Context, members func() []Target, o Options, out chan<- Event) {
	// One heartbeat snapshot per poll window, shared by every member stream.
	var snapMu sync.Mutex
	var snapAt time.Time
	var snap map[string]live.Entry
	o.Snap = func() map[string]live.Entry {
		snapMu.Lock()
		defer snapMu.Unlock()
		if time.Since(snapAt) > poll {
			snap = live.Snapshot()
			snapAt = time.Now()
		}
		return snap
	}

	var wg sync.WaitGroup
	defer wg.Wait() // the caller closes out after Group returns; no stream may outlive it
	running := map[string]context.CancelFunc{}
	startFile := map[string]string{} // the transcript each stream began with
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	launch := func(t Target) {
		cctx, cancel := context.WithCancel(ctx)
		running[t.ID], startFile[t.ID] = cancel, t.File
		wg.Add(1)
		go func() {
			defer wg.Done()
			Stream(cctx, t, o, out)
		}()
	}
	syncMembers := func() {
		for _, t := range members() {
			if cancel, ok := running[t.ID]; ok {
				// A member that registered before its transcript existed froze an
				// empty File; restart its stream now that the file is known.
				if startFile[t.ID] == "" && t.File != "" {
					cancel()
				} else {
					continue
				}
			}
			launch(t)
		}
	}
	syncMembers()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			syncMembers()
		}
	}
}

// waiting classifies an idle-after-output session as blocked when the harness's
// waiting_re matches the pane tail: auth when the tail shows a login URL, else a
// plain input prompt.
func waiting(t Target, o Options) (reason, hint string) {
	if t.WaitingRe == "" || o.PaneTail == nil {
		return "", ""
	}
	re, err := regexp.Compile(t.WaitingRe)
	if err != nil {
		return "", ""
	}
	tail := o.PaneTail(t.ID)
	if !re.MatchString(tail) {
		return "", ""
	}
	if m := authRe.FindString(tail); m != "" && strings.Contains(strings.ToLower(tail+m), "http") {
		return "auth", firstURL(tail)
	}
	return "input", lastLine(tail)
}

func preview(turns []session.NormTurn, withContent bool) string {
	if len(turns) == 0 {
		return ""
	}
	text := turns[len(turns)-1].Text
	text = strings.Join(strings.Fields(text), " ")
	if withContent || len(text) <= 200 {
		return text
	}
	return text[:199] + "…"
}

func firstURL(s string) string {
	if m := regexp.MustCompile(`https?://\S+`).FindString(s); m != "" {
		return m
	}
	return lastLine(s)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return strings.TrimSpace(lines[i])
		}
	}
	return ""
}
