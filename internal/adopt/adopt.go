// Package adopt correlates tmux windows running a harness that ax did not launch
// (no @ax_session tag, no session id in the start command) with the session
// running in them, so a hand-started session still shows live and focuses
// correctly instead of being duplicated when opened.
//
// The signal is the harness process's start time matched to the session's
// creation time in the same directory: a session created just after its window
// opened is the one that window started. It abstains whenever the match is
// ambiguous (no candidate, or more than one), so it never points a window at the
// wrong session.
package adopt

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/session"
)

// slack absorbs the gap between a window opening and the harness writing its
// first transcript line (startup, or the user typing the first prompt a moment
// later): a session counts as "started by" a pane if it was created no earlier
// than slack before the pane's process began.
const slack = 10 * time.Second

// Match returns the panes (keyed by session key) running sessions ax did not
// launch. located is the already-resolved id->locator map (from mux.Live): its
// keys are skipped as candidates and its locators mark panes already claimed, so
// only unresolved harness panes and sessions are considered. harnesses is the set
// of configured harness names, to tell a harness pane from a plain shell.
// procStart returns a pane process's start time (zero if unknown); it is injected
// so the matching is testable without spawning ps.
func Match(panes []mux.Pane, sessions []session.Session, located map[string]string, harnesses map[string]bool, procStart func(pid int) time.Time) map[string]mux.Pane {
	resolvedKey := map[string]bool{}
	resolvedLoc := map[string]bool{}
	for k, loc := range located {
		resolvedKey[k] = true
		resolvedLoc[loc] = true
	}

	// unresolved local sessions by directory, so a pane only considers sessions
	// that could plausibly be running in its cwd.
	byDir := map[string][]session.Session{}
	for _, s := range sessions {
		if s.Host != "" || resolvedKey[session.Key(s)] || s.Dir == "" || s.Created.IsZero() {
			continue
		}
		byDir[s.Dir] = append(byDir[s.Dir], s)
	}

	claims := map[string]int{}    // session key -> how many panes matched it
	pane := map[string]mux.Pane{} // session key -> the matching pane
	for _, p := range panes {
		if p.Tag != "" || p.Cwd == "" || resolvedLoc[p.Locator] || !runsHarness(p, harnesses) {
			continue
		}
		t0 := procStart(p.PID)
		if t0.IsZero() {
			continue
		}
		var only session.Session
		n := 0
		for _, s := range byDir[p.Cwd] {
			if s.Created.After(t0.Add(-slack)) { // created after this window opened
				only = s
				n++
			}
		}
		if n != 1 { // none, or ambiguous -> abstain
			continue
		}
		k := session.Key(only)
		claims[k]++
		pane[k] = p
	}

	out := map[string]mux.Pane{}
	for k, n := range claims {
		if n == 1 { // exactly one pane claimed this session; two+ is ambiguous
			out[k] = pane[k]
		}
	}
	return out
}

// Locators is mux.Live plus the hand-started matches: the full session-key ->
// locator map for BuildMeta. Returns nil when not inside the multiplexer.
func Locators(mx mux.Multiplexer, sessions []session.Session, harnesses map[string]bool) map[string]string {
	if !mx.Active() {
		return nil
	}
	loc := mx.Live()
	if loc == nil {
		loc = map[string]string{}
	}
	for k, p := range Match(mx.Panes(), sessions, loc, harnesses, ProcStart) {
		loc[k] = p.Locator
	}
	return loc
}

// runsHarness reports whether a pane looks like it is running a known harness, by
// matching a harness name as a token of its start command (e.g. "claude" or "cd
// dir && pi --session ...") or its current command.
func runsHarness(p mux.Pane, harnesses map[string]bool) bool {
	if harnesses[p.Cmd] {
		return true
	}
	for _, f := range strings.Fields(p.Start) {
		if harnesses[f] {
			return true
		}
	}
	return false
}

// procStarts memoizes ProcStart per pid: a process's start time never changes,
// and the picker's refresh timer would otherwise fork one `ps` per unmatched
// pane every second, forever.
var (
	procMu     sync.Mutex
	procStarts = map[int]time.Time{}
)

// ProcStart returns when process pid started, derived from `ps -o etime=`
// (elapsed time), or the zero time if it cannot be determined.
func ProcStart(pid int) time.Time {
	procMu.Lock()
	t, ok := procStarts[pid]
	procMu.Unlock()
	if ok {
		return t
	}
	t = time.Time{}
	if out, err := exec.Command("ps", "-o", "etime=", "-p", strconv.Itoa(pid)).Output(); err == nil {
		if d := parseETime(strings.TrimSpace(string(out))); d >= 0 {
			t = time.Now().Add(-d)
		}
	}
	procMu.Lock()
	procStarts[pid] = t
	procMu.Unlock()
	return t
}

// parseETime parses a ps elapsed-time field, "[[dd-]hh:]mm:ss", into a duration,
// or -1 if any part does not parse (so a malformed line never reads as "just
// started", which could cause a false match).
func parseETime(s string) time.Duration {
	if s == "" {
		return -1
	}
	days := 0
	if i := strings.IndexByte(s, '-'); i >= 0 {
		var err error
		if days, err = strconv.Atoi(s[:i]); err != nil {
			return -1
		}
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 1 || len(parts) > 3 {
		return -1
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return -1
		}
		nums[i] = n
	}
	d := time.Duration(days) * 24 * time.Hour
	units := []time.Duration{time.Hour, time.Minute, time.Second} // for len 3
	for i, n := range nums {
		d += time.Duration(n) * units[len(units)-len(nums)+i]
	}
	return d
}
