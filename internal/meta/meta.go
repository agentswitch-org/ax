// Package meta stores per-session control-layer metadata as a sidecar file, one
// per session id at $XDG_STATE_HOME/ax/meta/<id>.json. It is separate from the
// transcript (which ax reads but never writes) and from the live heartbeat, so
// there is no write contention: launch, tag, and outcome each touch only one
// id's file. session.Index merges it in.
package meta

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
)

// Meta is a session's control-layer metadata. Every field is optional; a session
// with no sidecar is the zero Meta.
type Meta struct {
	Name    string `json:"name,omitempty"`
	Task    string `json:"task,omitempty"`
	Group   string `json:"group,omitempty"`
	Parent  string `json:"parent,omitempty"`
	Origin  string `json:"origin,omitempty"`  // "human" | "agent"
	Mode    string `json:"mode,omitempty"`    // execution mode: "interactive" | "headless"
	Effort  string `json:"effort,omitempty"`  // reasoning effort level (low, medium, high, xhigh, max, ...)
	Harness string `json:"harness,omitempty"` // set at launch, for a pre-transcript row
	Dir     string `json:"dir,omitempty"`     // launch dir, for a pre-transcript row
	Outcome string `json:"outcome,omitempty"`
	// RecipePath/RecipeInterpreter/LogPath describe a tracked recipe root. The
	// recipe process is not backed by a harness transcript, so these fields let
	// the picker keep and preview the synthetic recipe row after the wrapper exits.
	RecipePath        string   `json:"recipe_path,omitempty"`
	RecipeInterpreter []string `json:"recipe_interpreter,omitempty"`
	LogPath           string   `json:"log_path,omitempty"`
	// FailReason is a short, single-line reason a headless session was marked
	// failed: either the harness's own known-fatal error signature (an
	// unsupported model, an auth failure, ...) or its last output on a non-zero
	// exit. Set alongside Outcome = "failure"; empty otherwise.
	FailReason string `json:"fail_reason,omitempty"`
	// Result is the session's final report: its last assistant message, snapshotted
	// into this durable record when the session concludes (the interactive done-gate
	// or Stop hook, or a headless process exit). It gives an interactive
	// subscription-auth worker the same machine-readable final output a headless
	// `claude -p` run prints to stdout, addressable by id via `ax result` and never
	// truncated, so a control-layer caller reads it without scraping the tmux pane.
	Result string `json:"result,omitempty"`
	// Exit is the session's exit code captured at conclude time: 0 for a clean
	// interactive conclusion, the harness's own exit code for a headless run. A
	// pointer so a session that has not concluded omits it rather than reporting a
	// misleading 0.
	Exit *int `json:"exit,omitempty"`
	// CloseOnDone tears the session down when a task-carrying interactive worker
	// concludes (--close-on-done), instead of halting it in the visible done state.
	CloseOnDone bool `json:"close_on_done,omitempty"`
	// KeepLive opts a parented task worker out of the default delayed reap, for
	// cases where a caller intends to keep sending it more input after conclusion.
	KeepLive bool `json:"keep_live,omitempty"`
	// KeepUntil is a keep-live lease deadline: while KeepLive is set and now is
	// before KeepUntil the worker is exempt from the post-conclude reap; once the
	// deadline passes it becomes reapable again (--keep-live-for DUR). A zero
	// KeepUntil with KeepLive set is an indefinite keep-live (--keep-live).
	KeepUntil time.Time `json:"keep_until,omitempty,omitzero"`
	// Spec is the persisted launch specification: everything ax needs to
	// reconstruct this session with `ax restart`. Written at launch; nil for a
	// session ax did not launch (a bare resume) or one launched before this
	// field existed, which is why restart refuses a session with no spec.
	Spec       *Spec     `json:"spec,omitempty"`
	Labels     []string  `json:"labels,omitempty"`
	Archived   bool      `json:"archived,omitempty"`
	ArchivedAt time.Time `json:"archived_at,omitempty,omitzero"`
	Updated    time.Time `json:"updated,omitempty"`
}

// Spec is a launch's full input, captured at launch so `ax restart` can rebuild
// the session identically: the behavior/model/task, the mode and fence flags, the
// accept check, and the environment and auth policy. It records the auth *policy*
// (subscription | api | env:VARNAME), never a secret, so a restart re-resolves the
// key from the named variable rather than persisting it. Group/Parent/Origin pin
// the session back into its original run.
type Spec struct {
	Harness      string   `json:"harness,omitempty"`
	Task         string   `json:"task,omitempty"`
	Behavior     string   `json:"behavior,omitempty"`      // the unresolved --behavior path, re-resolved on restart
	BehaviorText string   `json:"behavior_text,omitempty"` // explicit inline behavior text from --behavior-text
	Model        string   `json:"model,omitempty"`
	Name         string   `json:"name,omitempty"`
	Dir          string   `json:"dir,omitempty"`
	Accept       string   `json:"accept,omitempty"`
	Labels       []string `json:"labels,omitempty"` // explicit --label edits, re-folded over inheritance
	HFlags       []string `json:"hflags,omitempty"` // harness flags after `--`
	Group        string   `json:"group,omitempty"`
	Parent       string   `json:"parent,omitempty"`
	Origin       string   `json:"origin,omitempty"`

	Effort string `json:"effort,omitempty"` // reasoning effort level passed at launch (low, medium, high, xhigh, max, ...)

	// Mode and fence-policy intent (the flags, not the resolved command).
	Headless    bool `json:"headless,omitempty"`
	Wait        bool `json:"wait,omitempty"`
	Unattended  bool `json:"unattended,omitempty"`
	CloseOnDone bool `json:"close_on_done,omitempty"`
	KeepLive    bool `json:"keep_live,omitempty"`
	// KeepLiveFor is the --keep-live-for lease duration as launched, re-resolved to
	// a fresh absolute KeepUntil on restart (an absolute deadline would already be
	// stale by the time a restart runs). Empty for indefinite --keep-live.
	KeepLiveFor string   `json:"keep_live_for,omitempty"`
	WriteGlobs  []string `json:"write_globs,omitempty"`
	NoWrite     bool     `json:"no_write,omitempty"`
	FenceMode   string   `json:"fence_mode,omitempty"`

	// Environment and auth policy (see internal/app env choke point).
	CleanEnv bool     `json:"clean_env,omitempty"`
	Env      []string `json:"env,omitempty"` // KEY=VALUE overrides
	Auth     string   `json:"auth,omitempty"`

	// Fences.
	MaxCost    float64 `json:"max_cost,omitempty"`
	MaxTokens  float64 `json:"max_tokens,omitempty"`
	MaxWorkers int     `json:"max_workers,omitempty"`
	MaxDepth   int     `json:"max_depth,omitempty"`
	Timeout    string  `json:"timeout,omitempty"`

	// Self-propel: the ax outer loop that re-invokes an idle inline (pi/codex)
	// coordinator until the project is done, waiting on a human, or capped. Opt-in
	// via --self-propel; the run wrapper reads these off the persisted spec so the
	// pump survives a restart. See internal/propel (the run wrapper's pump).
	SelfPropel    bool   `json:"self_propel,omitempty"`     // the outer loop is engaged
	PropelPrompt  string `json:"propel_prompt,omitempty"`   // continue-prompt injected each idle turn ("" => built-in default)
	PropelDone    string `json:"propel_done,omitempty"`     // --propel-until/--done-check shell cmd; exit 0 => project complete
	PropelMaxIdle int    `json:"propel_max_idle,omitempty"` // consecutive no-progress turns before stopping (0 => default)
	PropelBackoff string `json:"propel_backoff,omitempty"`  // delay before re-injecting ("" => default)
	PropelWatch   string `json:"propel_watch,omitempty"`    // --propel-watch: file whose mtime change counts as progress ("" => none)
}

func dir() string { return axdir.State("meta") }

func path(id string) string { return filepath.Join(dir(), id+".json") }

func readDir() string { return axdir.StatePath("meta") }

func readPath(id string) string { return filepath.Join(readDir(), id+".json") }

// Load returns a session's metadata, or the zero Meta when it has no sidecar.
func Load(id string) Meta {
	var m Meta
	if data, err := os.ReadFile(readPath(id)); err == nil {
		json.Unmarshal(data, &m)
	}
	return m
}

// loadAll caches the last full scan, invalidated by the meta dir's mtime. Every
// write to a sidecar goes through axdir.WriteFileAtomic (temp-create + rename)
// or Remove, each of which bumps the directory's mtime; so an unchanged mtime
// means an unchanged set of sidecars and the prior result can be reused. This
// turns a repeated Index call (the picker refresh, fence pollers) from an
// O(sessions) storm of open+read+unmarshal per sidecar into a single stat.
var loadAll struct {
	sync.Mutex
	dir     string // guards against an XDG_STATE_HOME change (tests, federation)
	mtime   int64
	scanned int64 // wall clock of the cached scan, for the racy-mtime guard
	out     map[string]Meta
}

// mtimeSlack is how much older than its scan a dir mtime must be before an
// equal mtime proves the dir unchanged. Filesystem timestamps tick coarsely
// (observed ~10-15ms on NTFS, up to 2s on FAT): a write landing in the same
// tick as the scan leaves the mtime identical, so a scan that recent cannot
// vouch for the cache and LoadAll rescans instead (git's "racily clean" index
// problem, same cure).
const mtimeSlack = int64(2 * time.Second)

// LoadAll returns every session's metadata keyed by id, for a one-pass merge in
// session.Index. The whole scan is served from an in-process cache while the
// meta dir is unchanged, so a re-tag or outcome write (which bumps the dir
// mtime) is reflected on the next call without reparsing any transcript.
func LoadAll() map[string]Meta {
	d := readDir()

	loadAll.Lock()
	defer loadAll.Unlock()
	now := time.Now().UnixNano()
	if fi, err := os.Stat(d); err == nil {
		mt := fi.ModTime().UnixNano()
		if loadAll.out != nil && loadAll.dir == d && loadAll.mtime == mt && mt <= loadAll.scanned-mtimeSlack {
			return loadAll.out
		}
		defer func() { loadAll.dir, loadAll.mtime, loadAll.scanned = d, mt, now }()
	}

	es, err := os.ReadDir(d)
	if err != nil {
		loadAll.out = nil // don't serve a stale scan if the dir went away
		return map[string]Meta{}
	}
	out := make(map[string]Meta, len(es))
	for _, e := range es {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		var m Meta
		if data, err := os.ReadFile(filepath.Join(d, name)); err == nil {
			json.Unmarshal(data, &m)
		}
		out[strings.TrimSuffix(name, ".json")] = m
	}
	loadAll.out = out
	return out
}

// Save writes a session's metadata sidecar atomically, stamping Updated.
func Save(id string, m Meta) error {
	m.Updated = time.Now()
	if err := axdir.WriteJSON(path(id), m); err != nil {
		return err
	}
	invalidateLoadAll()
	return nil
}

// Update loads a session's metadata, applies fn, and writes it back. This is the
// idempotent read-modify-write behind `ax tag`.
func Update(id string, fn func(*Meta)) error {
	m := Load(id)
	fn(&m)
	return Save(id, m)
}

// SetArchived marks a session hidden-from-default-view or restores it. It only
// touches the metadata sidecar; transcripts and other session data stay intact.
func SetArchived(id string, archived bool) error {
	return Update(id, func(m *Meta) {
		if archived {
			if !m.Archived {
				m.ArchivedAt = time.Now()
			}
			m.Archived = true
			return
		}
		m.Archived = false
		m.ArchivedAt = time.Time{}
	})
}

// Remove deletes a session's sidecar (best-effort), for teardown.
func Remove(id string) {
	if err := os.Remove(readPath(id)); err == nil {
		invalidateLoadAll()
	}
}

func invalidateLoadAll() {
	loadAll.Lock()
	defer loadAll.Unlock()
	loadAll.out = nil
	loadAll.dir = ""
	loadAll.mtime = 0
	loadAll.scanned = 0
}
