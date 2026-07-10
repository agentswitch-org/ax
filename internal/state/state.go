// Package state computes a session's runtime state (live/crash, working/idle,
// and whether its project folder still exists) from the heartbeat files and the
// filesystem. It is headless: no multiplexer, no terminal. The TUI layers the
// viewer-window locator on top, and `ax list --json` serializes it for other
// hosts, so both share one source of truth.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/session"
)

func hookStateDir() string { return axdir.State("hookstate") }

func readHookStateDir() string { return axdir.StatePath("hookstate") }

func waitStateDir() string { return axdir.State("wait") }

func readWaitStateDir() string { return axdir.StatePath("wait") }

const waitMarkerFresh = 2 * time.Minute

// WriteHook records the authoritative activity a harness's own lifecycle hook
// reports (see `ax hook install`): "working", "idle", "blocked", or "done".
func WriteHook(id, s string) error {
	return axdir.WriteFileAtomic(filepath.Join(hookStateDir(), id), []byte(s), 0o600)
}

// RemoveHook clears a session's hook state (teardown), so a stale "working"
// never pins the activity of a later resume.
func RemoveHook(id string) { os.Remove(filepath.Join(readHookStateDir(), id)) }

// Blocked reports whether a session's own hook declared it blocked on the user
// (claude's Notification hook fires on permission prompts), so the picker can
// show "needs you" without scraping the pane.
func Blocked(id string) bool {
	hs, ok := hookState(id)
	return ok && hs == "blocked"
}

// Done reports whether a session's own hook declared it done: a task-carrying
// interactive worker whose task concluded (see `ax hookstate stop`). Like
// Blocked it is not aged out. it is a durable terminal marker the picker shows
// as "done" so a concluded worker never reads as a frozen idle session.
func Done(id string) bool {
	hs, ok := hookState(id)
	return ok && hs == doneState
}

// Failed reports whether a session was marked failed: a headless run that
// exited non-zero, or one whose own output matched a known-fatal error
// pattern (see app.ConcludeHeadless / app.MarkFailed). Like Done it is a
// durable terminal marker, distinct from it: done means the task concluded
// successfully, failed means it errored, so an agent polling `ax list
// --json` can tell the two apart instead of assuming a failed launch is still
// working.
func Failed(id string) bool {
	hs, ok := hookState(id)
	return ok && hs == failedState
}

// TerminalAt returns the mtime of a durable terminal hook marker. The timestamp
// is the source-state boundary used to decide whether a later same-session turn
// has reopened the task lifecycle.
func TerminalAt(id string) (time.Time, bool) {
	if !Terminal(id) {
		return time.Time{}, false
	}
	info, err := os.Stat(filepath.Join(readHookStateDir(), id))
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// Terminal reports whether a session has reached a durable terminal state: it
// concluded (Done) or errored (Failed). `ax wait` blocks on this, since both
// markers survive a --close-on-done teardown, so a caller can wait for a worker
// it launched without scraping the pane.
func Terminal(id string) bool { return Done(id) || Failed(id) }

// MarkWaiting records that owner is blocked in `ax wait` on child ids. The
// marker is owner-side and lightweight: it advertises orchestration state to the
// picker, and is cleared by the waiting process when it returns.
func MarkWaiting(owner string, ids []string) error {
	if owner == "" {
		return nil
	}
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		ClearWaiting(owner)
		return nil
	}
	return axdir.WriteJSON(filepath.Join(waitStateDir(), owner+".json"), waitMarker{IDs: out, OwnerPID: os.Getpid(), UpdatedAt: time.Now()})
}

// ClearWaiting removes an owner-side `ax wait` marker.
func ClearWaiting(owner string) {
	if owner == "" {
		return
	}
	os.Remove(filepath.Join(readWaitStateDir(), owner+".json"))
}

// WaitingOnChildren reports whether owner has a wait marker with at least one
// child id that has not reached a durable terminal state.
func WaitingOnChildren(owner string) bool {
	w, ok := readWaitMarker(owner)
	if !ok {
		return false
	}
	for _, id := range w.IDs {
		if !Terminal(id) {
			return true
		}
	}
	return false
}

type waitMarker struct {
	IDs       []string  `json:"ids"`
	OwnerPID  int       `json:"owner_pid,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

func readWaitMarker(owner string) (waitMarker, bool) {
	var w waitMarker
	if owner == "" {
		return w, false
	}
	data, err := os.ReadFile(filepath.Join(readWaitStateDir(), owner+".json"))
	if err != nil {
		return w, false
	}
	if json.Unmarshal(data, &w) != nil {
		return waitMarker{}, false
	}
	if len(w.IDs) == 0 || !validWaitMarker(w) {
		ClearWaiting(owner)
		return waitMarker{}, false
	}
	return w, true
}

func validWaitMarker(w waitMarker) bool {
	if w.OwnerPID <= 0 && w.UpdatedAt.IsZero() {
		return false
	}
	if w.OwnerPID > 0 && !processAlive(w.OwnerPID) {
		return false
	}
	if !w.UpdatedAt.IsZero() && time.Since(w.UpdatedAt) > waitMarkerFresh {
		return false
	}
	return true
}

// HookState returns a session's last hook-reported state ("working", "idle",
// "blocked", "done") and whether one was recorded. Exported so a choke point can
// edge-trigger on a transition (fire a notification only when blocked is new).
func HookState(id string) (string, bool) { return hookState(id) }

// hookState returns a session's hook-reported activity when one exists. It is
// preferred over output/transcript inference: a harness that reports its own
// state is authoritative (this is the more robust path, herdr-style).
func hookState(id string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(readHookStateDir(), id))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// Canonical state and activity values. view re-exports these for rendering and
// the wire format carries them verbatim, so a remote host and a local picker
// describe a session with the same words.
const (
	Live  = "live"  // fresh heartbeat: running now
	Crash = "crash" // a stale heartbeat: was running, died (recovery candidate)

	Working = "working" // produced terminal output recently
	Idle    = "idle"    // live but quiet (waiting for the user)

	LifecycleLive      = "live"
	LifecycleConcluded = "concluded"
	LifecycleCrashed   = "crashed"
	LifecycleDormant   = "dormant"

	DisplayLiveWorking      = "live-working"
	DisplayLiveWaiting      = "live-waiting"
	DisplayLiveDoneResident = "live-done-resident"
	DisplayConcluded        = LifecycleConcluded
	DisplayCrashed          = LifecycleCrashed
	DisplayDormant          = LifecycleDormant
)

// doneState is the hook-reported value a task-carrying interactive worker writes
// when its task concludes (see `ax hookstate stop`). It is not one of the
// spinner activity values (working/idle); the picker surfaces it via Runtime.Done.
const doneState = "done"

// failedState is the hook-reported value a headless run writes when it errors
// (a non-zero exit, or a known-fatal error pattern in its own output). Like
// doneState it is not a spinner activity value; the picker surfaces it via
// Runtime.Failed.
const failedState = "failed"

// Runtime is the computed-on-owner state of one session.
type Runtime struct {
	State     string // Live, Crash, or "" (inactive)
	Activity  string // Working, Idle, or "" (only set when live)
	DirExists bool   // the recorded project folder still exists
	Yolo      bool   // launched without guardrails (a --dangerously-* flag)
	Waiting   string // "input" (ax ask) or "children" (ax wait), computed by the owner
	Done      bool   // a task-carrying worker whose task concluded (`ax hookstate stop`)
	Failed    bool   // a headless run that errored (non-zero exit or a fatal output pattern)
	// TerminalAt is the mtime of the terminal (done/failed) hook marker: when the
	// task concluded. Zero unless Done or Failed. It is the warm-TTL and warm-set
	// sort key a coordinator reads off `ax list --json` as terminal_at.
	TerminalAt time.Time
}

// Lifecycle classifies the runtime into the one canonical lifecycle word shared
// by the picker and `ax list --json`.
func Lifecycle(r Runtime) string {
	switch {
	case r.Done || r.Failed:
		return LifecycleConcluded
	case r.State == Live:
		return LifecycleLive
	case r.State == Crash:
		return LifecycleCrashed
	default:
		return LifecycleDormant
	}
}

// DisplayPhase separates live process residence from task state for UI use. It
// deliberately does not replace Lifecycle, which remains the canonical storage
// and wire value.
func DisplayPhase(r Runtime) string {
	switch {
	case r.State == Live && (r.Done || r.Failed):
		return DisplayLiveDoneResident
	case r.State == Live && r.Activity == Working:
		return DisplayLiveWorking
	case r.State == Live:
		return DisplayLiveWaiting
	case r.Done || r.Failed:
		return DisplayConcluded
	case r.State == Crash:
		return DisplayCrashed
	default:
		return DisplayDormant
	}
}

// Ephemeral reports whether a session is a worker spawned by another session.
func Ephemeral(parent string) bool { return parent != "" }

// AutoRetireInput is the pure policy input for auto-archiving a safe session.
type AutoRetireInput struct {
	Parent      string
	Runtime     Runtime
	Last        time.Time
	Now         time.Time
	RetainAfter time.Duration
	PendingAsk  bool
	Archived    bool
}

// ShouldAutoRetire returns true only for concluded, non-live ephemeral sessions
// old enough to hide from default views. It never selects durable sessions, live
// sessions, already archived sessions, or sessions waiting on a human ask.
func ShouldAutoRetire(in AutoRetireInput) bool {
	if in.Archived || !Ephemeral(in.Parent) || in.PendingAsk {
		return false
	}
	if in.Runtime.State == Live || Lifecycle(in.Runtime) != LifecycleConcluded {
		return false
	}
	if in.Last.IsZero() {
		return false
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	if in.RetainAfter < 0 {
		return false
	}
	return !in.Last.After(now.Add(-in.RetainAfter))
}

// yoloFlags are the per-harness bypass-everything flags: claude/opencode's
// --dangerously-skip-permissions, codex's --dangerously-bypass-approvals-and-
// sandbox plus its --yolo/--full-auto shorthands, codex's explicit
// danger-full-access sandbox policy (how headless codex runs unattended), and
// claude's bypass mode.
var yoloFlags = []string{"--dangerously-", "--yolo", "--full-auto", "danger-full-access", "bypassPermissions"}

// IsYolo reports whether a launch command runs the agent with no permission
// gate, so the picker can flag an unsupervised session (⚠). It matches known
// bypass flags in the command itself, ignoring single-quoted values (the task
// text), so a prompt that merely mentions a flag is not flagged.
func IsYolo(cmd string) bool {
	cmd = stripQuoted(cmd)
	for _, f := range yoloFlags {
		if strings.Contains(cmd, f) {
			return true
		}
	}
	return false
}

// stripQuoted removes single-quoted segments (the shell quoting fillTemplate
// applies to values like the task), leaving only the command and its flags. The
// escaped-apostrophe idiom '\” is neutralized first, or its three quote chars
// would flip the in/out parity and hide (or expose) the rest of the command.
func stripQuoted(s string) string {
	s = strings.ReplaceAll(s, `'\''`, "\x00")
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == '\'' {
			in = !in
			continue
		}
		if !in {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ComputeAll returns the runtime state of every session, keyed by id. It reads
// the heartbeat snapshot once and stats each distinct directory once.
func ComputeAll(sessions []session.Session) map[string]Runtime {
	snap := live.Snapshot()
	dirOK := map[string]bool{}
	out := make(map[string]Runtime, len(sessions))
	for _, s := range sessions {
		var r Runtime
		if s.Dir != "" {
			ok, seen := dirOK[s.Dir]
			if !seen {
				ok = config.DirExists(s.Dir)
				dirOK[s.Dir] = ok
			}
			r.DirExists = ok
		}
		e, beat := snap[s.ID]
		switch {
		case beat && live.Running(e):
			r.State = Live
		case beat:
			r.State = Crash
		}
		if r.State == Live {
			r.Activity = activityOf(s, e, beat)
		}
		r.Yolo = beat && live.Running(e) && IsYolo(e.Cmd)
		r.Done = Done(s.ID)     // a concluded worker's hook marker, independent of the heartbeat
		r.Failed = Failed(s.ID) // an errored headless run's hook marker, independent of the heartbeat
		if r.Done || r.Failed {
			// terminal_at: the mtime of the marker just read, which is when the task
			// concluded. Stat only when terminal, so a live working session pays nothing.
			if info, err := os.Stat(filepath.Join(readHookStateDir(), s.ID)); err == nil {
				r.TerminalAt = info.ModTime()
			}
		}
		if r.State == Live && WaitingOnChildren(s.ID) {
			r.Waiting = "children"
		}
		out[s.ID] = r
	}
	return out
}

// HookFresh bounds how long a harness's own hook state is trusted for the
// working/idle spinner over the output/mtime heuristic. The hook is
// edge-triggered (working on turn start, idle on Stop), so a "working" state is
// valid for the whole turn no matter how long the harness thinks without writing
// a transcript line: that is the point of preferring it. Past this window an
// unclosed state (a Stop that never fired because the harness was killed) is
// treated as stale, and activity falls back to the accurate output/mtime signal.
// The window is generous because that fallback keeps a genuinely working session
// lit, so aging the hook out is safe.
const HookFresh = 15 * time.Minute

// FileActivity decides working vs idle from the transcript's mtime alone. It is
// for a session that is live by an open viewer window but has no heartbeat (so
// the access point has no last-output time), where a recently written transcript
// still means the harness is working. A fresh hook-reported state wins over the
// mtime, since the harness is authoritative about its own turn.
func FileActivity(s session.Session) string {
	if hs, ok := hookActivityFresh(s.ID); ok {
		return hs
	}
	if info, err := os.Stat(s.File); err == nil && recent(info.ModTime()) {
		return Working
	}
	return Idle
}

// hookActivity maps a hook-reported state to the working/idle the picker shows.
func hookActivity(hs string) string {
	if hs == "working" {
		return Working
	}
	return Idle
}

// hookActivityFresh reads a session's hook-reported activity for the spinner, but
// only while the hook state is fresh (see HookFresh). ok is false when there is
// no hook state or it has gone stale, so the caller falls back to the output/mtime
// heuristic. Freshness is the hook file's mtime: it is rewritten on every
// lifecycle transition, so an unclosed "working" ages out while a live session's
// real work keeps it lit through the fallback. Blocked (needs-you) is surfaced
// separately by Blocked and is intentionally not aged out here.
func hookActivityFresh(id string) (string, bool) {
	path := filepath.Join(readHookStateDir(), id)
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) > HookFresh {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return hookActivity(strings.TrimSpace(string(data))), true
}

// activityOf decides working vs idle by the union of two signals: the ax-run
// wrapper's last-output time (accurate, from terminal writes) and the transcript's
// mtime. Either being recent means working. The union keeps the spinner lit for
// harnesses whose pty output is bursty (a gap during a tool call) but which stream
// to their transcript, and vice versa, instead of flickering to idle mid-work.
func activityOf(s session.Session, e live.Entry, beat bool) string {
	if hs, ok := hookActivityFresh(s.ID); ok { // a harness's own hook is authoritative
		return hs
	}
	if beat && e.LastOutput > 0 && recent(time.Unix(e.LastOutput, 0)) {
		return Working
	}
	if info, err := os.Stat(s.File); err == nil && recent(info.ModTime()) {
		return Working
	}
	return Idle
}

// recent reports whether t is within the working window (output or a transcript
// write this recent counts as actively working).
func recent(t time.Time) bool { return !t.IsZero() && time.Since(t) < live.Active }
