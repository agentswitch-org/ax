// Package finder is the interactive selector: a self-rendered terminal UI (see
// picker.go / term.go) that owns the screen and its own animation loop. fzf is
// used only as the fuzzy-match engine (see picker.fuzzy); ripgrep does content
// search and the view package renders. The Finder interface keeps the app
// decoupled from all of it.
package finder

import (
	"errors"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/models"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
	"github.com/agentswitch-org/ax/internal/view"
)

// ErrNoSessions is returned by Pick when a background load (View.Load)
// completes and finds nothing to show, so the caller can print its "no
// sessions" message the same way it would have before the picker's data
// gathering moved off the startup path.
var ErrNoSessions = errors.New("no sessions found")

// Finder shows sessions and returns the user's choice.
type Finder interface {
	// Pick presents the session table and returns the selection.
	Pick(View) (Choice, error)
	// Choose is a simple single-select list. header and prompt may be empty;
	// expect names extra accept keys, and the pressed one is returned (empty for
	// Enter). item is empty when the user aborts.
	Choose(prompt, header string, items, expect []string) (item, key string, err error)
	// ChooseDir is Choose with live candidates: browse(query) is called on every
	// edit to supply the current options (e.g. the subdirs of a typed path), so
	// the list follows you through the filesystem. expect/return work as Choose.
	ChooseDir(prompt, header string, browse func(query string) []string, expect []string) (item, key string, err error)
	// Prompt reads a single line of free text, pre-filled with initial and
	// editable (empty if cleared or aborted).
	Prompt(label, initial string) (string, error)
	// PromptMultiline reads a multiline block of free text: Enter inserts a
	// newline, Ctrl-D accepts, Esc/Ctrl-C cancels. ok is false on cancel; the
	// composed task/behavior text is returned on accept (may be empty).
	PromptMultiline(label, header, initial string) (text string, ok bool)
}

// View is the data the finder renders.
type View struct {
	Config   config.Config
	Models   models.DB
	Sessions []session.Session
	Meta     map[string]view.RowMeta // per-session live state (STATE/ACT/WIN)
	// RemoteState is the owner-reported runtime state of remote sessions, keyed
	// by id. The picker uses it on refresh so a remote session keeps its host's
	// reported state instead of being recomputed against local heartbeats (which
	// would read it as inactive).
	RemoteState map[string]state.Runtime
	// Hosts is the machine roster (local plus each configured host with its
	// status), shown in the machines line and used by the host filter. Nil when
	// no hosts are configured.
	Hosts []view.HostStatus
	// OnKill stops the given sessions. The picker calls it inline (after its own
	// confirm) so a kill refreshes the list instead of leaving the picker.
	OnKill func([]session.Session)
	// OnArchive applies reversible archive-state edits for the given sessions.
	// The picker decides the target state per row (archive visible rows,
	// unarchive archived rows), calls this inline, and updates successful rows in
	// its in-memory list so they move between archive views immediately. The
	// returned map is keyed by session.Key; missing keys are treated as success.
	OnArchive func([]ArchiveChange) map[string]error
	// OnDetachWindows closes the viewer windows of the given sessions, detaching a
	// dtach-held session so its process survives (reopening reattaches it). The
	// picker calls it inline on the bulk-detach key. The callback is responsible
	// for skipping any session whose window is not detach-safe (closing it would
	// kill the process); the picker only reports the resulting window-count delta.
	OnDetachWindows func([]session.Session)
	// OnOpenWindows reopens the viewer windows of the given sessions, reattaching a
	// detached (held) session to its live process or resuming one with no window.
	// The picker calls it inline on the bulk-open key.
	OnOpenWindows func([]session.Session)
	// RemotePreview fetches a remote session's rendered preview over its host's
	// transport. The picker calls it off the UI goroutine and caches the result,
	// so a remote row's body loads lazily on selection. Nil disables remote
	// preview (the "local-only" placeholder is shown instead).
	RemotePreview func(session.Session) []string
	// RemoteSearch returns the set of remote session keys ("host/id") whose
	// transcript matches query, by running the content search on each host over
	// its transport. The picker calls it off the UI goroutine, debounced, in
	// content-search mode. Nil disables remote search (local sessions only).
	RemoteSearch func(query string) map[string]bool
	// Reindex re-reads the config and re-scans local transcripts, returning a
	// fresh, fully-composed snapshot (federated sessions + column-augmented
	// config) so an open picker reflects new/removed sessions and config edits
	// live. The picker calls it off the UI goroutine on a throttled poll tick.
	// Nil disables live refresh (the picker only updates live state on the tick).
	Reindex func() ([]session.Session, config.Config)
	// Load, when set, is run once in a goroutine right after the picker opens
	// its screen, so the first frame paints immediately (a lightweight loading
	// state) instead of blocking on session federation, tmux queries, and any
	// configured host's network round-trip. Its result replaces this (empty)
	// View once ready, exactly as if it had been passed in from the start. A
	// loaded View with no Sessions and no HostUpdates means "nothing to
	// show": Pick then closes the picker and returns ErrNoSessions, mirroring
	// the pre-async early return. Every other field on the initial View is
	// ignored when Load is set; the loaded View is authoritative.
	Load func() View
	// NetHosts lists the configured host names the network/health panel (key
	// `S`) fans out to. Nil or empty disables the panel (nothing to show).
	NetHosts func() []string
	// NetStatusHost fetches one host's live status row (reachability + latency,
	// ax/wire version + compat, OS/shell, harnesses, profile drift) over its
	// transport, reusing the same fold `ax config status` uses. Slow (ssh), so
	// the panel calls it off the UI goroutine, one goroutine per host.
	NetStatusHost func(name string) view.NetHostStatus
	// NetSync pushes the local profile to one host (the `ax config sync --host`
	// path) and returns a one-line result; NetSyncAll does every configured
	// host. The panel confirms in-overlay before calling either.
	NetSync    func(name string) string
	NetSyncAll func() string
	// NetRollback restores a host's latest config backup (the `ax config
	// rollback --host` path) and returns a one-line result. Confirm-gated.
	NetRollback func(name string) string
	// HostUpdates, when set on the loaded View, streams federation into the
	// open picker: Load returns as soon as local sessions are indexed (keys
	// unblock there), and each configured host's result lands here as its
	// fetch completes, merged like a live reindex, so an unreachable host
	// fills in late (as offline) instead of gating the first interactive
	// frame on its timeout. Closed once every host has reported; a loaded
	// View with no Sessions keeps the loading skeleton until the first
	// update that brings rows, and returns ErrNoSessions if the channel
	// closes with nothing to show anywhere.
	HostUpdates <-chan HostUpdate
}

// ArchiveChange is one picker-requested archive-state edit.
type ArchiveChange struct {
	Session  session.Session
	Archived bool
}

// HostUpdate is one streamed federation delivery: the full recomposed view
// after a host's fetch completed (merged sessions, column-augmented config,
// owner-reported remote state, and the machine roster with that host's
// status resolved). Full snapshots rather than per-host deltas, so the
// picker's merge is exactly its live-reindex path.
type HostUpdate struct {
	Sessions    []session.Session
	Config      config.Config
	RemoteState map[string]state.Runtime
	Hosts       []view.HostStatus
}

// Choice is what the user picked.
type Choice struct {
	Picked   []session.Session // selected rows
	Compose  bool              // user opened the compose flow (c/C): harness + mode + dir
	New      bool              // user asked for a fresh session, no args (`ax new`)
	NewArgs  bool              // fresh session with the harness's configured args (`ax new --with-args`)
	WithArgs bool              // resume applying the harness's configured args (E)
	// Config, Hosts, and Sessions are the picker's final loaded state (post
	// Load, post any live reindex), so a caller using View.Load can keep acting
	// on the choice (new session, resume, correlating a hand-started window)
	// without re-loading what Pick already gathered.
	Config   config.Config
	Hosts    []view.HostStatus
	Sessions []session.Session
}

// New returns the TUI finder. m supplies multiplexer state (pane moves, live
// windows) the picker reads.
func New(m mux.Multiplexer) Finder { return &tui{mx: m} }

type tui struct {
	mx        mux.Multiplexer
	lastFrame []string // the picker's final frame, the backdrop for follow-up choosers
}

func (t *tui) Pick(v View) (Choice, error) {
	if v.Load == nil && len(v.Sessions) == 0 {
		return Choice{}, nil
	}
	meta := v.Meta
	if meta == nil {
		meta = map[string]view.RowMeta{}
	}
	p := &picker{cfg: v.Config, db: v.Models, mx: t.mx, all: v.Sessions, meta: meta, remoteState: v.RemoteState, hosts: v.Hosts, onKill: v.OnKill, onArchive: v.OnArchive, onDetachWindows: v.OnDetachWindows, onOpenWindows: v.OnOpenWindows, remotePreview: v.RemotePreview, remoteSearch: v.RemoteSearch, reindex: v.Reindex, load: v.Load}
	choice, err := p.run()
	if p.sc != nil {
		t.lastFrame = p.frameLines() // backdrop for a follow-up chooser (`c new`)
	}
	return choice, err
}

func (t *tui) Choose(prompt, header string, items, expect []string) (string, string, error) {
	return runChoose(prompt, header, items, nil, expect, t.lastFrame)
}

func (t *tui) ChooseDir(prompt, header string, browse func(string) []string, expect []string) (string, string, error) {
	return runChoose(prompt, header, nil, browse, expect, t.lastFrame)
}

func (t *tui) Prompt(label, initial string) (string, error) {
	return runPrompt(label, initial), nil
}

func (t *tui) PromptMultiline(label, header, initial string) (string, bool) {
	return runPromptMultiline(label, header, initial)
}
