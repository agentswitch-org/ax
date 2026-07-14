// Package app orchestrates the commands using the tool interfaces. It indexes
// sessions, drives the finder, and resumes or launches through the multiplexer.
// It never imports fzf/rg/tmux/zoxide directly, only their interfaces.
package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agentswitch-org/ax/internal/adopt"
	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/dirs"
	"github.com/agentswitch-org/ax/internal/finder"
	"github.com/agentswitch-org/ax/internal/hold"
	hostreg "github.com/agentswitch-org/ax/internal/hosts"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/models"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/remap"
	"github.com/agentswitch-org/ax/internal/retention"
	"github.com/agentswitch-org/ax/internal/search"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/shell"
	"github.com/agentswitch-org/ax/internal/state"
	"github.com/agentswitch-org/ax/internal/view"
	"github.com/agentswitch-org/ax/internal/wire"
)

// Deps are the tool implementations the app drives.
type Deps struct {
	Find finder.Finder
	Mux  mux.Multiplexer
	Dirs dirs.Source
}

// App is the orchestrator.
type App struct {
	find finder.Finder
	mux  mux.Multiplexer
	dirs dirs.Source
}

func New(d Deps) App { return App{find: d.Find, mux: d.Mux, dirs: d.Dirs} }

func muxHasWindows(m mux.Multiplexer) bool {
	return m != nil && m.Active() && m.HasWindows()
}

func harnessByName(harnesses []config.Harness) map[string]config.Harness {
	byName := map[string]config.Harness{}
	for _, h := range harnesses {
		byName[h.Name] = h
	}
	return byName
}

func hostsByName(hosts []config.Host) map[string]config.Host {
	byName := map[string]config.Host{}
	for _, h := range hosts {
		byName[h.Name] = h
	}
	return byName
}

// Pick opens the picker and acts on the choice: jump into a running session's
// window, resume one into a fresh window, fan several into background windows,
// or fall through to a new session. The picker's screen opens and paints its
// first frame immediately; loadPickView (session federation, tmux queries, any
// configured host's network round-trip) runs off the UI goroutine, behind a
// loading skeleton, so a slow or unreachable host never stalls the picker's
// startup (see finder.View.Load).
func (a App) Pick() {
	// The federated fan-out (and any in-picker remote callback) shells out to
	// each host while the alt-screen is up, so missing-shell warnings are routed
	// to the ax log for the picker's lifetime. That keeps them out of the live
	// frame and off the shell prompt on exit (including Ctrl+C), while still
	// recording them for `ax log`; a targeted `ax <harness> --host X` still warns
	// to stderr.
	restore := logShellWarnings()
	choice, err := a.find.Pick(finder.View{Load: a.loadPickView})
	restore()
	if errors.Is(err, finder.ErrNoSessions) {
		fmt.Fprintln(os.Stderr, "ax: no sessions found in this view. Start one with 'ax new' or 'ax claude \"your task\"'; use 'ax list --all' for archived local sessions or 'ax list --federated --all' for configured hosts.")
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
	a.act(choice)
}

// act carries out the picker's choice: start a new session, focus/open/resume a
// picked one, or (no multiplexer) drop straight into a single pick or print the
// resume commands for several. Split from Pick so the routing is unit-testable
// without driving the TUI.
func (a App) act(choice finder.Choice) {
	cfg := choice.Config
	byName := harnessByName(cfg.Harnesses)
	hostByName := hostsByName(cfg.Hosts)
	if choice.Compose {
		a.compose(cfg, choice.Hosts, hostByName)
		return
	}
	if choice.New {
		a.newSession(cfg, choice.Hosts, hostByName, false)
		return
	}
	if choice.NewArgs {
		a.newSession(cfg, choice.Hosts, hostByName, true)
		return
	}
	if len(choice.Picked) == 0 {
		return
	}

	// Resume flags. Enter and `e` resume clean; `E` (WithArgs) applies each
	// harness's configured args. A nil override means "each harness's own args"
	// (E); a non-nil "" is forced on every pick, so the clean keys strip any flags
	// a config parked in args.
	var override *string
	if !choice.WithArgs {
		empty := ""
		override = &empty
	}

	// Single pick: jump into the session (switch to its window if already open,
	// else resume it in a focused window). Multiple picks fan out in the
	// background, skipping any already up. The process backend is excluded here
	// even though it reports Active(): it has no window to focus (its Open only
	// spawns the holder detached in the background), so for an interactive single
	// pick that would leave the terminal at a blank prompt with nothing attached.
	// It falls through instead to the in-terminal attach path below, which
	// create-or-attaches the session's holder in this very terminal.
	if len(choice.Picked) == 1 && muxHasWindows(a.mux) {
		s := choice.Picked[0]
		key := attachWindowKeyFrom(s, choice.Sessions)
		if win, ok := locateAttachWindowFrom(a.mux, s, choice.Sessions); ok {
			if err := a.mux.Focus(win); err == nil {
				return
			} else {
				axlog.Printf("focus %s: %v", key, err)
			}
		}
		if p, ok := a.adoptedWindow(cfg, choice.Sessions, s); ok { // a window ax did not launch, correlated by adopt
			if err := a.mux.Focus(p.Window); err == nil {
				return
			} else {
				axlog.Printf("focus adopted %s: %v", key, err)
			}
		}
		s = canonicalAttachSessionFrom(s, choice.Sessions)
		key = attachWindowKeyFrom(s, choice.Sessions)
		target := muxTargetFor(s.Labels, cfg.MuxGroup)
		if s.Host != "" { // remote: attach over the transport (no local cd/relink)
			if err := a.mux.Open("", view.WindowTitle(s), a.remoteAttachCmd(hostByName[s.Host], s.ID, override), key, target, true); err != nil {
				reportAttachError(key, "open tmux window", err)
			}
			return
		}
		dir, ok := a.resolveDir(cfg, s)
		if !ok {
			return
		}
		s.Dir = dir
		cmd := resumeCmd(byName[s.Harness], s, override)
		if err := localAttachPreflight(s.ID, cmd); err != nil {
			reportAttachError(s.ID, "open tmux window", err)
			return
		}
		if err := a.mux.Open(s.Dir, view.WindowTitle(s), heldWindowCmd(s.ID, cmd), key, target, true); err != nil {
			reportAttachError(s.ID, "open tmux window", err)
		}
		return
	}
	// Single pick with no window multiplexer (none active, or the process backend
	// which has no window): drop the user straight into the live harness in this
	// terminal (returning to the shell on exit) rather than printing the resume
	// command or spawning it detached. This is the `ax attach` machinery: a local
	// session goes through execHeld, which reattaches the held process behind
	// its socket (a running session is resumed in place, not restarted) or
	// resumes clean when none survives; a remote session exec-replaces into its
	// host's `ax attach` over the transport. Multiple picks fall through to the
	// print loop below (one terminal can't exec into several).
	if len(choice.Picked) == 1 {
		s := canonicalAttachSessionFrom(choice.Picked[0], choice.Sessions)
		if s.Host != "" {
			execRemoteAttachFn(a.remoteAttachCmd(hostByName[s.Host], s.ID, override))
			return
		}
		dir, ok := a.resolveDir(cfg, s)
		if !ok {
			return
		}
		s.Dir = dir
		cmd := resumeCmd(byName[s.Harness], s, override)
		if err := localAttachPreflight(s.ID, cmd); err != nil {
			reportAttachError(s.ID, "attach", err)
			return
		}
		execHeldFn(s.ID, cmd)
		return
	}
	for _, s := range choice.Picked {
		if a.mux.Active() {
			if _, ok := locateAttachWindowFrom(a.mux, s, choice.Sessions); ok {
				continue
			}
		}
		s = canonicalAttachSessionFrom(s, choice.Sessions)
		key := attachWindowKeyFrom(s, choice.Sessions)
		target := muxTargetFor(s.Labels, cfg.MuxGroup)
		if s.Host != "" {
			cmd := a.remoteAttachCmd(hostByName[s.Host], s.ID, override)
			if !a.mux.Active() {
				fmt.Println(cmd)
				continue
			}
			if err := a.mux.Open("", view.WindowTitle(s), cmd, key, target, false); err != nil {
				reportAttachError(key, "open tmux window", err)
			}
			continue
		}
		dir, ok := a.resolveDir(cfg, s)
		if !ok {
			continue
		}
		s.Dir = dir
		cmd := resumeCmd(byName[s.Harness], s, override)
		if err := localAttachPreflight(s.ID, cmd); err != nil {
			reportAttachError(s.ID, "open tmux window", err)
			continue
		}
		if !a.mux.Active() {
			fmt.Println(cmd)
			continue
		}
		if err := a.mux.Open(s.Dir, view.WindowTitle(s), heldWindowCmd(s.ID, cmd), key, target, false); err != nil {
			reportAttachError(s.ID, "open tmux window", err)
		}
	}
}

func canonicalAttachSessionFrom(s session.Session, sessions []session.Session) session.Session {
	if s.Host == "" {
		if !hasExactLocalSession(sessions, s.ID) {
			s.ID = resolveIDFromSessions(s.ID, sessions)
		}
	}
	return s
}

func attachWindowKeyFrom(s session.Session, sessions []session.Session) string {
	return session.Key(canonicalAttachSessionFrom(s, sessions))
}

func attachWindowKeysFrom(s session.Session, sessions []session.Session) []string {
	key := session.Key(s)
	if s.Host != "" {
		return []string{key}
	}
	real := resolveIDFromSessions(s.ID, sessions)
	if real == "" || real == s.ID {
		return []string{key}
	}
	if hasExactLocalSession(sessions, s.ID) {
		return []string{key}
	}
	return []string{real, key}
}

func locateAttachWindowFrom(m mux.Multiplexer, s session.Session, sessions []session.Session) (string, bool) {
	for _, key := range attachWindowKeysFrom(s, sessions) {
		if win, ok := m.Locate(key); ok {
			return win, true
		}
	}
	return "", false
}

func hasExactLocalSession(sessions []session.Session, id string) bool {
	if id == "" {
		return false
	}
	for _, s := range sessions {
		if s.Host == "" && s.ID == id {
			return true
		}
	}
	return false
}

func localAttachPreflight(id, cmd string) error {
	held := hold.Available() && hold.Probe(id)
	if e, ok := live.Snapshot()[id]; ok && live.Running(e) && !held {
		return fmt.Errorf("session is live but no holder answers and no tmux window was found; refusing to open a duplicate")
	}
	if strings.TrimSpace(cmd) == "" && !held {
		return fmt.Errorf("no holder answers and the harness has no resume command")
	}
	return nil
}

func reportAttachError(id, action string, err error) {
	fmt.Fprintf(os.Stderr, "ax: attach %s: %s: %v\n", id, action, err)
}

// loadPickView gathers everything the picker needs to render: the local
// sessions, the composed column layout, and live/adopted window state. It is
// passed as finder.View.Load, so it runs off the UI goroutine and the picker's
// first frame never blocks on it (a transcript scan or a tmux query). Host
// federation never blocks it either: the fan-out starts here, but each host's
// result streams into the open picker via View.HostUpdates as its fetch
// returns, so time-to-interactive is bounded by the local scan regardless of
// how slow or unreachable a configured host is. A dead host degrades to "its
// roster entry flips to offline late", not "the picker drops keys for its
// whole timeout".
func (a App) loadPickView() finder.View {
	cfg, _ := config.Load()
	cfg.Hosts = hostreg.Merge(cfg.Hosts) // fold in self-registered ephemeral hosts
	db := models.Load()
	base := cfg // pre-composition, the fallback when a snapshot's config reload fails
	sessions := indexedSessions(cfg)
	remoteState := map[string]state.Runtime{}
	var fed *federation
	var hosts []view.HostStatus
	var hostUpdates chan finder.HostUpdate
	if len(cfg.Hosts) > 0 {
		fed = newFederation(cfg.Hosts)
		hosts = fed.roster(len(sessions))
		hostUpdates = make(chan finder.HostUpdate, len(cfg.Hosts))
		var wg sync.WaitGroup
		for _, h := range cfg.Hosts {
			wg.Add(1)
			go func(h config.Host) {
				defer wg.Done()
				ss, rt, st := fetchHost(h)
				if st != view.HostOnline {
					axlog.Printf("host %s: %s", h.Name, st)
				}
				fed.record(h.Name, hostResult{sessions: ss, rt: rt, state: st})
				// Buffered to the host count, so this never blocks even if the
				// picker has already closed.
				hostUpdates <- a.fedSnapshot(base, fed)
			}(h)
		}
		go func() { wg.Wait(); close(hostUpdates) }() // closed = every host reported
	}
	if len(sessions) == 0 && fed == nil {
		return finder.View{} // no Sessions tells Pick to report "no sessions found"
	}
	cfg = composeCols(cfg, sessions, false)
	hostByName := hostsByName(cfg.Hosts)
	byName := harnessByName(cfg.Harnesses)
	// Correlate windows ax did not launch with the session running in them, so a
	// hand-started session shows live and, when picked, focuses its window instead
	// of resuming a duplicate. One mux.Live() listing feeds both adopt and the
	// locator map (each call is a tmux exec).
	loc := a.live()
	var adopted map[string]mux.Pane
	if muxHasWindows(a.mux) {
		adopted = adopt.Match(a.mux.Panes(), sessions, loc, harnessNames(cfg), adopt.ProcStart)
	}
	meta := finder.BuildMeta(sessions, a.mergeAdopted(loc, adopted), remoteState)
	d := view.DefaultSortCol(cfg)
	view.Sort(cfg, sessions, db, d, view.DefaultDescFor(cfg, d), meta)

	var remoteSearch func(string) map[string]bool
	if fed != nil {
		// The roster is read at call time, not captured: a host that streams in
		// online after the picker opened is searched, one that timed out is not.
		remoteSearch = func(q string) map[string]bool { return a.remoteSearch(hostByName, fed.roster(0), q) }
	}
	// Live refresh: re-read the config and re-scan local transcripts on the
	// picker's poll tick, so new/removed sessions and new [[harness]] entries
	// appear without a relaunch. Remote sessions are not re-polled (that is a
	// per-tick transport round-trip); the set streamed in at open is carried
	// over so they neither vanish nor stall the loop.
	reindex := func() ([]session.Session, config.Config) {
		hu := a.fedSnapshot(base, fed)
		return hu.Sessions, hu.Config
	}
	// Network/health panel wiring: the configured [[host]] roster feeds the panel,
	// each row is a status fetch reusing computeHostStatus (the `ax config status`
	// fold), and the mutations drive the sync/rollback code paths non-interactively
	// (the panel confirms in-TUI). localHash is read at open; the panel re-queries a
	// row after a sync/rollback so a fresh drift verdict reflects the change.
	var netHostNames []string
	for _, h := range cfg.Hosts {
		netHostNames = append(netHostNames, h.Name)
	}
	netTargets := append([]config.Host(nil), cfg.Hosts...)
	localHash := config.ProfileHash(cfg.Profile())
	var detachWindows func([]session.Session)
	var openWindows func([]session.Session)
	if muxHasWindows(a.mux) {
		sessionContext := func(picked []session.Session) []session.Session {
			ctx := append([]session.Session(nil), sessions...)
			return append(ctx, picked...)
		}
		detachWindows = func(picked []session.Session) { a.detachWindows(picked, sessionContext(picked)) }
		openWindows = func(picked []session.Session) { a.openWindows(cfg, byName, hostByName, picked, sessionContext(picked)) }
	}
	return finder.View{
		Config: cfg, Models: db, Sessions: sessions, Meta: meta,
		RemoteState: remoteState, Hosts: hosts, HostUpdates: hostUpdates,
		OnKill: func(picked []session.Session) { a.killSessions(hostByName, picked) },
		OnArchive: func(changes []finder.ArchiveChange) map[string]error {
			return a.applyArchiveChanges(hostByName, fed, changes)
		},
		OnDetachWindows: detachWindows,
		OnOpenWindows:   openWindows,
		RemotePreview:   func(s session.Session) []string { return a.remotePreview(hostByName, s) },
		RemoteSearch:    remoteSearch,
		Reindex:         reindex,
		NetHosts:        func() []string { return netHostNames },
		NetStatusHost:   func(name string) view.NetHostStatus { return computeHostStatus(hostByName[name], localHash) },
		NetSync:         func(name string) string { return a.netSyncHost(hostByName[name]) },
		NetSyncAll:      func() string { return a.netSyncAll(netTargets) },
		NetRollback:     func(name string) string { return a.netRollbackHost(hostByName[name]) },
	}
}

func (a App) applyArchiveChanges(hostByName map[string]config.Host, fed *federation, changes []finder.ArchiveChange) map[string]error {
	errs := map[string]error{}
	for _, ch := range changes {
		s := ch.Session
		key := session.Key(s)
		var err error
		if s.Host == "" {
			if ch.Archived {
				// The picker already confirmed this archive interactively, so the
				// --force guard (meant for non-interactive `ax archive`) is redundant
				// double-gating here.
				err = archiveSession(s.ID, true, nil)
			} else {
				err = meta.SetArchived(s.ID, false)
			}
		} else {
			h, ok := hostByName[s.Host]
			if !ok {
				err = fmt.Errorf("unknown host %q", s.Host)
			} else {
				verb := "unarchive"
				if ch.Archived {
					verb = "archive"
				}
				err = archiveRemoteSession(h, verb, s.ID)
				if err == nil && fed != nil {
					fed.setArchived(s.Host, s.ID, ch.Archived)
				}
			}
		}
		if err != nil {
			errs[key] = err
		}
	}
	return errs
}

func archiveRemoteSession(h config.Host, verb, id string) error {
	prog, argv := remoteArgv(h, verb, []string{id}, false)
	if prog == "" {
		return fmt.Errorf("empty transport for host %q", h.Name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteVerbTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, prog, argv...).CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("transport to %s timed out", h.Name)
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return err
	}
	return fmt.Errorf("%v: %s", err, msg)
}

// adoptedWindow finds the window a hand-started (untagged) pane is already
// running s in, so a single pick focuses it instead of resuming a duplicate.
// Recomputed here at the moment of selection rather than carried from the
// picker's background load (it needs a tmux pane listing and a ps per
// unmatched pane, and by the time the user has picked something the load is
// long done anyway). sessions must be the full loaded set, not just s: Match
// abstains when a pane's directory+timing match is ambiguous between several
// sessions, and that check only works with every candidate in view.
func (a App) adoptedWindow(cfg config.Config, sessions []session.Session, s session.Session) (mux.Pane, bool) {
	matched := adopt.Match(a.mux.Panes(), sessions, a.live(), harnessNames(cfg), adopt.ProcStart)
	p, ok := matched[session.Key(s)]
	return p, ok
}

// remotePreview fetches a remote session's rendered preview over its host's
// transport (`ax preview <id>`), for the picker to show a remote body on demand.
// It blocks on the transport, so the picker calls it off the UI goroutine.
func (a App) remotePreview(hostByName map[string]config.Host, s session.Session) []string {
	h, ok := hostByName[s.Host]
	if !ok {
		return []string{"", "  unknown host: " + s.Host}
	}
	prog, argv := transportArgv(h, "preview", s.ID)
	out, err := exec.Command(prog, argv...).Output()
	if err != nil {
		axlog.Printf("preview %s on %s: %v", s.ID, s.Host, err)
		return []string{"", "  preview unavailable from " + s.Host + " (see ax log)"}
	}
	text := strings.ReplaceAll(string(out), "\r\n", "\n")
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

// killSessions stops each chosen session (closing a window only detaches; this is
// the explicit teardown). Local sessions are signalled directly; remote ones
// route `ax kill` over their host's transport. The picker confirms before calling
// this and stays open afterward, so it takes no screen of its own.
func (a App) killSessions(hostByName map[string]config.Host, picked []session.Session) {
	for _, s := range picked {
		var err error
		if s.Host != "" {
			prog, argv := transportArgv(hostByName[s.Host], "kill", s.ID)
			if out, e := exec.Command(prog, argv...).CombinedOutput(); e != nil {
				err = fmt.Errorf("%v: %s", e, strings.TrimSpace(string(out)))
			}
		} else {
			err = live.Kill(s.ID)
			MarkKilled(s.ID)
			killCleanup(s.ID)
		}
		if err != nil {
			axlog.Printf("kill %s: %v", s.ID, err)
		}
	}
}

// detachWindows closes each selected session's viewer window, detaching a
// held session so its harness process survives (reopening reattaches it).
// A session whose window is NOT detach-safe is skipped: closing it would kill the
// process (verified: an unheld tmux window's process dies with kill-window), and
// detach must never kill. The picker reports the skip via its window-count delta.
func (a App) detachWindows(picked, sessions []session.Session) {
	if !muxHasWindows(a.mux) {
		return
	}
	for _, s := range picked {
		if !windowDetachSafe(s) {
			continue // unheld local window: closing it would kill the process, so leave it up
		}
		key := attachWindowKeyFrom(s, sessions)
		for _, candidate := range attachWindowKeysFrom(s, sessions) {
			if _, ok := a.mux.Locate(candidate); ok {
				key = candidate
				break
			}
		}
		if err := a.mux.CloseWindow(key); err != nil {
			axlog.Printf("detach window %s: %v", key, err)
		}
	}
}

// windowDetachSafe reports whether closing s's viewer window merely detaches it
// (the process survives) rather than killing the process. A remote session is
// held on its owner, so closing the local attach window only drops the view: the
// session keeps running there. A local session is detach-safe only when a holder
// answers on its socket (hold.Probe): a live dial is the ground-truth held check,
// where a stat cannot tell a live holder from a stale socket file. It also
// excludes the process backend (persists on its own; there is no holder layer)
// and the none backend / a missing dtach binary (hold.Available).
func windowDetachSafe(s session.Session) bool {
	if s.Host != "" {
		return true // remote: the session lives on its owner; closing only detaches the view
	}
	if mux.IsProcess() || !hold.Available() {
		return false
	}
	return hold.Probe(resolveID(s.ID))
}

// openWindows reopens each selected session's viewer window in the background,
// the inverse of detachWindows. It reuses the same resume/attach path Pick's
// multi-pick fan-out takes: an already-open window is left alone, a remote
// session re-attaches over its transport, and a local one relaunches held so it
// lands back on its holder socket. Because a held session's holder outlives the
// closed window, create-or-attach reattaches the same live process instead of
// restarting the harness; a session with no surviving holder resumes clean.
func (a App) openWindows(cfg config.Config, byName map[string]config.Harness, hostByName map[string]config.Host, picked, sessions []session.Session) {
	if !muxHasWindows(a.mux) {
		return
	}
	empty := ""
	for _, s := range picked {
		if _, ok := locateAttachWindowFrom(a.mux, s, sessions); ok {
			continue // already open: leave focus where it is
		}
		s = canonicalAttachSessionFrom(s, sessions)
		key := attachWindowKeyFrom(s, sessions)
		target := muxTargetFor(s.Labels, cfg.MuxGroup)
		if s.Host != "" {
			if err := a.mux.Open("", view.WindowTitle(s), a.remoteAttachCmd(hostByName[s.Host], s.ID, &empty), key, target, false); err != nil {
				axlog.Printf("open window %s: %v", key, err)
			}
			continue
		}
		if s.Dir != "" && !config.DirExists(s.Dir) {
			continue // folder gone: skip rather than prompt for a relink while the picker owns the screen
		}
		cmd := resumeCmd(byName[s.Harness], s, &empty)
		if err := localAttachPreflight(s.ID, cmd); err != nil {
			axlog.Printf("open window %s: %v", key, err)
			continue
		}
		if err := a.mux.Open(s.Dir, view.WindowTitle(s), heldWindowCmd(s.ID, cmd), key, target, false); err != nil {
			axlog.Printf("open window %s: %v", key, err)
		}
	}
}

// resolveDir returns the live working directory for a session, prompting for a
// new location (and remembering it) when the recorded folder no longer exists.
// ok is false only when the dir is gone and the user aborts the relink, so the
// caller can skip that session. An empty dir is left as-is (no cd needed).
func (a App) resolveDir(cfg config.Config, s session.Session) (string, bool) {
	if config.DirExists(s.Dir) || s.Dir == "" {
		return s.Dir, true
	}
	var cands []string
	for _, d := range a.candidateDirs(cfg) {
		if config.DirExists(d) {
			cands = append(cands, d)
		}
	}
	// Browse-enabled like the new-session dir picker: the moved-to folder is
	// often not in any candidate list yet (nothing has run there), so typing
	// / or ~ must be able to walk the filesystem to it.
	browse := func(query string) []string { return browseDirs(query, cands) }
	header := "folder gone: " + s.Dir + "   pick its new location (type / or ~ to browse, esc to skip)"
	sel, _, err := a.find.ChooseDir("relink "+filepath.Base(s.Dir)+"❯ ", header, browse, nil)
	if err != nil || strings.TrimSpace(sel) == "" {
		return s.Dir, false
	}
	nd := config.ExpandHome(sel)
	remap.Add(s.Dir, nd)
	return nd, true
}

// heartbeat wraps a resume command so the window runs it under "ax run", which
// records a heartbeat while the harness lives (for live detection and crash
// recovery). --hold keeps the window open showing the error when the harness
// dies within seconds of launch, instead of flashing away; --wait/detached runs
// don't pass it (nobody is watching, and a block would hang the caller).
func heartbeat(id, cmd string) string {
	return runWrapperShellCommand(id, cmd, "", true)
}

// heldWindowCmd is the shell command a tmux window runs to start (or reattach) a
// session, held so closing the window detaches rather than kills. Native: the
// window runs the thin `ax attach --cmd` client, which create-or-attaches the
// detached `ax run` holder (the held process is the heartbeat wrapper, so
// liveness and working/idle keep updating even while detached). The dtach
// backend keeps the old wrap; none (or a missing dtach) degrades to the unheld
// heartbeat wrap, where closing the window kills the session.
func heldWindowCmd(id, cmd string) string {
	if mux.IsProcess() {
		return heartbeat(id, cmd)
	}
	switch hold.Backend() {
	case hold.BackendNative:
		return attachWrapperShellCommand(id, cmd, "")
	case hold.BackendDtach:
		if hold.Available() {
			return fmt.Sprintf("dtach -A %s %s", shellQuote(hold.Sock(id)), heartbeat(id, cmd))
		}
	}
	return heartbeat(id, cmd)
}

// heldAdoptCmd is heldWindowCmd for a harness that mints its own session id
// (codex, opencode): the window is held under a placeholder endpoint (axid) and
// `ax run --adopt` discovers the real id from the index after launch, then
// heartbeats under it, aliases the endpoint, and re-tags the window. Without a
// holder it degrades to the unheld adopt wrapper.
func heldAdoptCmd(axid, harness, cmd string) string {
	inner := runWrapperShellCommand(axid, cmd, harness, true)
	if mux.IsProcess() {
		return inner
	}
	switch hold.Backend() {
	case hold.BackendNative:
		return attachWrapperShellCommand(axid, cmd, harness)
	case hold.BackendDtach:
		if hold.Available() {
			return fmt.Sprintf("dtach -A %s %s", shellQuote(hold.Sock(axid)), inner)
		}
	}
	return inner
}

// execHeldAdopt is execHeld for a mint-its-own-id harness (the not-in-tmux and
// remote `ax new` path): hold under a placeholder endpoint and let `ax run
// --adopt` discover and adopt the real id.
func execHeldAdopt(axid, harness, cmd string) {
	ax := self()
	if hold.Backend() == hold.BackendNative {
		execReplaceFn(ax, append([]string{ax}, attachWrapperArgs(axid, cmd, harness)...), os.Environ())
	}
	run := runWrapperArgsWith(axid, cmd, harness, true)
	if hold.Backend() == hold.BackendDtach {
		if dtach, ok := hold.Path(); ok {
			execReplaceFn(dtach, append([]string{"dtach", "-A", hold.Sock(axid), ax}, run...), os.Environ())
		}
	}
	if err := execReplaceFn(ax, append([]string{ax}, run...), os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
}

// remoteAttachCmd is the shell command a local tmux window runs to attach a
// session living on host h: it runs `ax attach <id>` over the transport, and the
// remote ax does the holding. Local and remote attach differ only by this
// transport prefix. The transport must allocate a pty (e.g. "ssh -t box").
func (a App) remoteAttachCmd(h config.Host, id string, override *string) string {
	ax := h.Ax
	if ax == "" {
		ax = "ax"
	}
	// Quote the ax command for the remote shell, then quote the whole thing again
	// for the local shell that runs the transport. ssh flattens its args before
	// the remote shell re-parses them, so two quote levels (local shell, then the
	// remote shell) are needed for a multi-word --args override to survive intact.
	// The inner level is always POSIX: the remote host's shell parses it, not the
	// local platform's; only the outer level follows the local shell.
	remote := ax + " attach " + shell.QuotePosix(id)
	if override != nil {
		remote += " --args " + shell.QuotePosix(*override)
	}
	return h.Transport + " " + shellQuote(remote)
}

// Attach reconstructs the resume command for an existing session and becomes
// its viewer (create-or-attach against the holder), so the calling window shows
// the held session. This is the federation delegation point: a viewer runs "ax
// attach <id>" locally, or "ssh -t HOST ax attach <id>" to attach a session
// living on another machine. Optional "--args <flags>" overrides the harness's
// default launch flags. The internal "--cmd <command>" form (what a held tmux
// window runs) skips the index lookup and holds that exact command under the
// id, with "--adopt <harness>" for a mint-its-own-id launch; the session may
// not exist yet on that path.
func (a App) Attach(args []string) {
	id, override, cmd, hasCmd, adopt, parseErr := parseAttachArgs(args)
	if parseErr != nil {
		fmt.Fprintln(os.Stderr, "ax:", parseErr)
		exitFn(2)
		return
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: ax attach <id> [--args <flags>]")
		exitFn(2)
		return
	}
	var cfg config.Config
	var sessions []session.Session
	loadedIndex := false
	loadIndex := func() {
		if loadedIndex {
			return
		}
		cfg, _ = config.Load()
		sessions = session.Index(cfg)
		loadedIndex = true
	}
	// A present --cmd takes the viewer path even when its value is empty (a
	// harness with no resume template): falling through to the index lookup
	// would rebuild the same empty command and spawn `ax attach --cmd ''`
	// again, a process loop. Empty means attach-only (nothing to spawn).
	if hasCmd {
		if adopt == "" {
			loadIndex()
			if !hasExactLocalSession(sessions, id) {
				id = resolveIDFromSessions(id, sessions)
			}
		}
		// The dedicated viewer process (execHeld exec-replaces into this form):
		// run the native attach client directly. This process owns the terminal
		// alone, which the client requires (see execHeld).
		if hold.Backend() == hold.BackendNative {
			attachHolderFn(id, cmd, adopt) // exits with the detach or harness code
		}
		if adopt != "" {
			execHeldAdopt(id, adopt, cmd)
		} else {
			execHeld(id, cmd)
		}
		return
	}
	loadIndex()
	byName := harnessByName(cfg.Harnesses)
	for _, s := range sessions {
		if s.ID == id {
			cmd := resumeCmd(byName[s.Harness], s, override)
			if err := localAttachPreflight(s.ID, cmd); err != nil {
				reportAttachError(s.ID, "attach", err)
				exitFn(1)
				return
			}
			execHeldFn(s.ID, cmd)
			return
		}
	}
	realID := resolveIDFromSessions(id, sessions)
	if realID != id {
		for _, s := range sessions {
			if s.ID == realID {
				cmd := resumeCmd(byName[s.Harness], s, override)
				if err := localAttachPreflight(s.ID, cmd); err != nil {
					reportAttachError(s.ID, "attach", err)
					exitFn(1)
					return
				}
				execHeldFn(s.ID, cmd)
				return
			}
		}
	}
	fmt.Fprintf(os.Stderr, "ax: no session %q\n", id)
	exitFn(1)
}

// parseAttachArgs pulls the id, an optional "--args <flags>" override, and the
// internal "--cmd <command>" / "--cmd-file <path>" / "--adopt <harness>" pair
// out of the attach argv.
// hasCmd distinguishes a present-but-empty --cmd (viewer mode, attach-only)
// from no --cmd at all (reconstruct the command from the index).
func parseAttachArgs(args []string) (id string, override *string, cmd string, hasCmd bool, adopt string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--args":
			if i+1 < len(args) {
				v := args[i+1]
				override = &v
				i++
			}
			continue
		case "--cmd":
			hasCmd = true
			if i+1 < len(args) {
				cmd = args[i+1]
				i++
			}
			continue
		case "--cmd-file":
			hasCmd = true
			if i+1 >= len(args) {
				return id, override, cmd, hasCmd, adopt, fmt.Errorf("--cmd-file needs a path")
			}
			path := args[i+1]
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return id, override, cmd, hasCmd, adopt, fmt.Errorf("--cmd-file %s: %w", path, readErr)
			}
			_ = os.Remove(path)
			cmd = string(data)
			i++
			continue
		case "--adopt":
			if i+1 < len(args) {
				adopt = args[i+1]
				i++
			}
			continue
		}
		if id == "" {
			id = args[i]
		}
	}
	return
}

// execHeldFn, execHeldAdoptFn, execRemoteAttachFn, and execRemoteNewFn are the
// exec-into-harness actions the direct attach, no-mux single-select, and
// new-session paths take. They are package vars so a test can assert the routing
// decision without exec-replacing the test process.
var (
	execHeldFn         = execHeld
	execHeldEnvFn      = execHeldEnv
	execHeldAdoptFn    = execHeldAdopt
	execRemoteAttachFn = execRemoteAttach
	execRemoteNewFn    = execRemoteNew
	execReplaceFn      = execReplace
	attachHolderFn     = attachHolder
	exitFn             = os.Exit
)

// execRemoteAttach exec-replaces this process with cmd, the remote attach
// command (e.g. "ssh -t host ax attach <id>"), so a no-mux single pick of a
// remote session drops the user into the session over its host's transport and
// returns to the shell on exit. The remote ax does the holding, so a running
// remote session is reattached, not restarted. It is the remote-session mirror
// of execHeld.
func execRemoteAttach(cmd string) {
	path, argv := shell.ExecReplaceArgs(cmd)
	if err := execReplace(path, argv, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
}

// execRemoteNew exec-replaces this process with cmd, the remote new-session
// command (e.g. "ssh -t host ax new"), so a no-mux remote new drops the user into
// the remote harness picker and launcher over its host's transport.
func execRemoteNew(cmd string) {
	path, argv := shell.ExecReplaceArgs(cmd)
	if err := execReplaceFn(path, argv, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
}

// execHeld turns this process into the session's viewer: create-or-attach
// semantics against the holder. Native: exec-replace into a fresh `ax attach
// <id> --cmd` process, which runs the in-process attach client (the holder is
// a detached `ax run`, spawned when absent). The exec is load-bearing: the
// picker's shared /dev/tty reader (finder's decodeLoop) lives for the process
// lifetime, and an attach client running inside the picker process races it
// for every keystroke (keys eaten, the Ctrl-G detach byte never seen).
// execve wipes those goroutines and hands the client sole ownership of the
// terminal, exactly as the dtach exec always did. dtach: exec dtach -A as
// before. Without a holder it runs the heartbeat wrapper unheld. It never
// returns except on failure to exec.
func execHeld(id, cmd string) {
	execHeldEnv(id, cmd, nil)
}

func execHeldEnv(id, cmd string, extraEnv []string) {
	ax := self()
	env := mergeEnv(os.Environ(), extraEnv)
	if hold.Backend() == hold.BackendNative {
		execReplaceFn(ax, []string{ax, "attach", id, "--cmd", cmd}, env)
	}
	if hold.Backend() == hold.BackendDtach {
		if dtach, ok := hold.Path(); ok {
			execReplaceFn(dtach, append([]string{"dtach", "-A", hold.Sock(id), ax}, runWrapperArgsWith(id, cmd, "", true)...), env)
		}
	}
	if err := execReplaceFn(ax, append([]string{ax}, runWrapperArgsWith(id, cmd, "", true)...), env); err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
}

func mergeEnv(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	out := base[:0:0]
	keys := map[string]bool{}
	for _, kv := range extra {
		if k, _, ok := strings.Cut(kv, "="); ok {
			keys[k] = true
		}
	}
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok && keys[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// attachHolder runs the native attach client against session id (see attachPty),
// exiting with the detach (0) or harness exit code. It does not return.
func attachHolder(id, cmd, adopt string) {
	os.Exit(attachPty(id, cmd, adopt))
}

// attachPty runs the native attach client against session id, spawning the
// detached `ax run` holder (setsid: it survives this client, the window, and
// the terminal) when none is running yet. An empty cmd means attach-only
// (nothing to hold if no holder answers). It returns the detach (0) or harness
// exit code, or 1 when the client cannot attach.
func attachPty(id, cmd, adopt string) int {
	var spawn func() error
	if cmd != "" {
		spawn = attachSpawn(id, cmd, adopt)
	}
	// The menu chord (Ctrl-A then a) detaches then reopens the picker: bare `ax`
	// opens the TUI, and exec-replacing into it hands this terminal to the picker
	// (the picker owns /dev/tty alone, which an in-process attach client would
	// race). It returns only if exec fails, leaving the client to report a plain
	// detach.
	openMenu := func() {
		ax := self()
		if err := execReplaceFn(ax, []string{ax}, os.Environ()); err != nil {
			fmt.Fprintln(os.Stderr, "ax:", err)
		}
	}
	code, err := hold.Attach(id, spawn, openMenu)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		return 1
	}
	return code
}

// attachSpawn builds the create side of attachPty's create-or-attach.
func attachSpawn(id, cmd, adopt string) func() error {
	return func() error {
		c := exec.Command(self(), runWrapperArgs(id, cmd, adopt)...)
		setDetached(c) // its own session: no terminal, no group signal reaches it
		return startAndReap(c)
	}
}

func startAndReap(c *exec.Cmd) error {
	if err := c.Start(); err != nil {
		return err
	}
	go c.Wait()
	return nil
}

// Kill stops running sessions by id (the "ax kill <id>" verb). Closing a viewer
// window only detaches; this ends the process. It is the remote teardown point:
// ssh HOST ax kill <id>. Ids resolve through the launch alias, so the id a
// launch printed keeps working for a harness that minted its own (codex). A
// task worker killed before it concluded is marked failed, so a waiter holding
// its id unblocks with a truthful outcome instead of hanging forever.
func (a App) Kill(args []string) {
	// A host-qualified id (host/id) or --host reruns the kill on that host (main's
	// dispatch already stripped --run via GroupArg, so dehost only sees ids). The
	// picker's killSessions routes its own remote kills; this is the CLI path.
	if host, rest := dehost(args); host != "" {
		a.remoteVerb("kill", host, rest, false)
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ax kill <id>...")
		os.Exit(2)
	}
	cfg, _ := config.Load()
	for _, id := range args {
		id = resolveID(id)
		killErr := live.Kill(id)
		fallbackKilled, fallbackErr := a.killAdoptedWorkerOrphanIfSafe(id, cfg)
		if fallbackErr != nil {
			fmt.Fprintf(os.Stderr, "ax: kill %s: %v\n", id, fallbackErr)
		} else if killErr != nil && !fallbackKilled {
			fmt.Fprintf(os.Stderr, "ax: kill %s: %v\n", id, killErr)
		}
		MarkKilled(id)
		killCleanup(id)
	}
}

// killCleanup clears the sidecars a dead session must not keep asserting: its
// pending question (or the picker screams "needs you" at a corpse forever) and
// its hook-reported activity. A durable terminal marker (done/failed) is NOT
// activity and survives: it is the record that the task concluded, and wiping
// it would turn a finished worker back into a corpse for `ax wait` and the
// picker (the exact bug that made success read as failure after a cleanup kill).
func killCleanup(id string) {
	ask.Remove(id)
	if !state.Terminal(id) {
		state.RemoveHook(id)
	}
}

// resolveID follows launch aliases and unambiguous local session-id prefixes, so
// every verb accepts the id a launch printed and the short ID shown in the picker.
func resolveID(id string) string {
	return newIDResolver().resolve(id)
}

type idResolver struct {
	sessions []session.Session
}

func newIDResolver() idResolver {
	cfg, _ := config.Load()
	return idResolver{sessions: session.Index(cfg)}
}

func (r idResolver) resolve(id string) string {
	return resolveIDFromSessions(id, r.sessions)
}

func resolveIDFromSessions(id string, sessions []session.Session) string {
	if id == "" {
		return ""
	}
	// A real local session id beats stale alias metadata.
	if hasExactLocalSession(sessions, id) {
		return id
	}
	if aliased := meta.ResolveAlias(id); aliased != id {
		return aliased
	}
	if m := prefixMatches(sessions, id); len(m) == 1 {
		return m[0].ID
	}
	return id
}

func self() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "ax"
}

// shellQuote quotes a value as a single shell literal, the single quoting
// authority for every command string app builds (dtach/tmux/transport chains and
// the env prefix). It delegates to the platform shell layer so the same call
// sites emit POSIX or PowerShell quoting; on unix it is byte-for-byte the prior
// single-quote escaping.
func shellQuote(s string) string { return shell.Quote(s) }

// quoteVal shell-quotes a value substituted into a harness command template so a
// transcript-derived field (dir, id, model) can't inject shell. An empty value
// stays empty rather than becoming ”, so e.g. `cd {dir}` with no recorded dir
// stays `cd ` (falls back to $HOME) instead of erroring on `cd ”`.
func quoteVal(s string) string {
	if s == "" {
		return ""
	}
	return shellQuote(s)
}

// New starts a fresh session: pick a harness, pick or create a directory, and
// land in the new window. name preselects the harness ("" prompts for it, used by
// `ax new <harness>`). explicit overrides the launch flags (from `ax new <harness>
// <flags...>`); with no explicit flags, withArgs applies the harness's configured
// `args` (the picker's C) and otherwise the session launches clean (c).
func (a App) New(name string, withArgs bool, explicit *string) {
	cfg, _ := config.Load()
	if name == "" {
		var names []string
		for _, h := range cfg.Harnesses {
			names = append(names, h.Name)
		}
		var err error
		name, _, err = a.find.Choose("", "harness", names, nil)
		if err != nil || name == "" {
			return
		}
	}
	var harness config.Harness
	for _, h := range cfg.Harnesses {
		if h.Name == name {
			harness = h
		}
	}
	if harness.Name == "" {
		fmt.Fprintf(os.Stderr, "ax: unknown harness %q\n", name)
		return
	}
	dir, ok := a.pickDir(cfg, harness.Name)
	if !ok {
		return
	}
	args := ""
	switch {
	case explicit != nil:
		args = *explicit
	case withArgs:
		args = harness.Args
	}

	// Mint the session id up front when the harness can be told to use one
	// ({newid} in its launch template, e.g. claude --session-id). That lets ax
	// tag the window and hold a heartbeat from the start, so the new session shows
	// live and re-selecting it focuses its window instead of opening a duplicate.
	// Without {newid} support the id is unknown until the first transcript write,
	// so the window stays untracked (the pre-fix behavior).
	newid := ""
	if strings.Contains(harness.Launch, "{newid}") {
		newid = newUUID()
	}
	launch := launchCmd(harness, &args, newid)

	// A harness without {newid} mints its own session id (codex, opencode), so ax
	// can't tag the window with an id yet. Hold it under a placeholder and let
	// `ax run --adopt` discover the real id from the index once the harness writes
	// its transcript, then heartbeat, socket-alias, and re-tag under it. claude and
	// pi take the {newid} path and are tracked from the first frame.
	adoptID := ""
	if newid == "" {
		adoptID = newUUID()
	}

	if muxHasWindows(a.mux) {
		cmd, tag := launch, newid
		switch {
		case newid != "":
			cmd = heldWindowCmd(newid, launch)
		case adoptID != "":
			cmd, tag = heldAdoptCmd(adoptID, harness.Name, launch), adoptID
		}
		// A fresh interactive new has no seeded labels yet (it does not go through
		// runLaunch), so derive the project label from the dir to group it like any
		// other launch. A non-"project" key has no label here, yielding "" (flat).
		target := muxTargetFor(seedProjectLabel(nil, dir), cfg.MuxGroup)
		a.mux.Open(dir, harness.Name, cmd, tag, target, true)
		return
	}
	// Not in tmux: launch in place in the current terminal (this is also the path
	// `ssh -t host ax new` takes on the target machine). cd into the dir, then exec
	// the held launch so the session persists like any other.
	if dir != "" {
		os.Chdir(dir)
	}
	switch {
	case newid != "":
		execHeldFn(newid, launch)
	case adoptID != "":
		execHeldAdoptFn(adoptID, harness.Name, launch)
	default:
		path, argv := shell.ExecReplaceArgs(launch)
		if err := execReplaceFn(path, argv, os.Environ()); err != nil {
			fmt.Fprintln(os.Stderr, "ax:", err)
		}
	}
}

// newSession starts a fresh session, first asking which machine when hosts are
// configured. Local runs the interactive New here; a remote target runs `ax new`
// on the host over its transport, so the host picks the harness and directory
// and launches there (held, streamed back). Real multiplexers open that transport
// in a local window; no-window backends exec or print it in this terminal.
func (a App) newSession(cfg config.Config, hosts []view.HostStatus, hostByName map[string]config.Host, withArgs bool) {
	target, ok := a.chooseTarget(hosts)
	if !ok {
		return
	}
	if target == "" {
		a.New("", withArgs, nil)
		return
	}
	a.runRemoteNew(target, a.remoteNewCmd(hostByName[target], withArgs))
}

// chooseTarget asks which machine a fresh launch runs on: local plus any online
// host. It asks only when there is a real choice; a single reachable target (or
// every host offline) resolves straight to local. target is "" for local and a
// host name for remote; ok is false when the user aborts the machine chooser.
func (a App) chooseTarget(hosts []view.HostStatus) (target string, ok bool) {
	var names []string
	for _, h := range hosts {
		if h.State == view.HostLocal || h.State == view.HostOnline {
			names = append(names, h.Name)
		}
	}
	if len(names) <= 1 {
		return "", true
	}
	sel, _, _ := a.find.Choose("new session on ❯ ", "", names, nil)
	if sel == "" {
		return "", false
	}
	if sel == "local" {
		return "", true
	}
	return sel, true
}

// runRemoteNew starts a bootstrap `ax new` on host over its transport: a real
// multiplexer opens it in a local window (the real session, and its grouping, is
// born on the host, so no local mux target), a no-window active backend execs it
// in this terminal, and the none backend prints it for the user to run.
func (a App) runRemoteNew(target, cmd string) {
	if muxHasWindows(a.mux) {
		a.mux.Open("", target+"·new", cmd, "", "", true)
	} else if a.mux.Active() {
		execRemoteNewFn(cmd)
	} else {
		fmt.Println(cmd)
	}
}

// remoteNewCmd is the shell command a local window or terminal exec runs to start
// a new session on host h: `ax new` over the transport, so the host picks the
// harness and dir and launches there. withArgs applies the chosen harness's
// configured flags.
func (a App) remoteNewCmd(h config.Host, withArgs bool) string {
	ax := h.Ax
	if ax == "" {
		ax = "ax"
	}
	inner := ax + " new"
	if withArgs {
		inner += " --with-args"
	}
	return h.Transport + " " + shellQuote(inner)
}

// pickDir fuzzy-finds a parent directory. Enter uses it as-is; Tab makes the
// highlighted directory a base and prompts for a subdir to create under it.
func (a App) pickDir(cfg config.Config, harness string) (string, bool) {
	cands := a.candidateDirs(cfg)
	browse := func(query string) []string { return browseDirs(query, cands) }
	sel, key, err := a.find.ChooseDir(harness+" dir❯ ",
		"enter: use directory   tab: make a subdir   type / or ~ to browse", browse, []string{"tab"})
	if err != nil || sel == "" {
		return "", false
	}
	dir := config.ExpandHome(sel)
	if key == "tab" {
		entered, _ := a.find.Prompt(fmt.Sprintf("new subdir under %s (blank = use as-is)", dir), "")
		sub := strings.TrimSpace(entered)
		if sub != "" {
			dir = filepath.Join(dir, sub)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintln(os.Stderr, "ax:", err)
				return "", false
			}
		}
	}
	return dir, true
}

// browseDirs supplies the dir-picker candidates for the current query: the base
// (frecent and session dirs), plus live filesystem browsing. An empty query lists
// home's subdirs so an unfamiliar machine isn't blank; a query that looks like a
// path lists the subdirs of its deepest typed component. Entries keep the typed
// form (~ or /) so fuzzy matching and later ExpandHome both work; dotdirs show
// only when the trailing component starts with a dot.
func browseDirs(query string, base []string) []string {
	return browseDirsWithReadDir(query, base, func(prefix string) ([]fs.DirEntry, error) {
		return os.ReadDir(config.ExpandHome(prefix))
	})
}

func browseDirsWithReadDir(query string, base []string, readDir func(string) ([]fs.DirEntry, error)) []string {
	out := append([]string{}, base...)
	q := strings.TrimSpace(query)
	prefix, leaf, ok := browsePrefix(q)
	if !ok {
		return out
	}
	entries, err := readDir(prefix)
	if err != nil {
		return out
	}
	showHidden := q != "" && strings.HasPrefix(leaf, ".")
	for _, e := range entries {
		if e.IsDir() && (showHidden || !strings.HasPrefix(e.Name(), ".")) {
			out = append(out, prefix+e.Name())
		}
	}
	return out
}

func browsePrefix(q string) (prefix, leaf string, ok bool) {
	switch {
	case q == "":
		return "~/", "", true
	case strings.HasPrefix(q, "/"), strings.HasPrefix(q, "~"), isWindowsPathQuery(q):
		sep := lastPathSeparator(q)
		if sep >= 0 {
			return q[:sep+1], q[sep+1:], true
		}
		if isWindowsDriveQuery(q) {
			return q, "", true
		}
	}
	return "", "", false
}

func isWindowsPathQuery(q string) bool {
	return isWindowsDriveQuery(q) || strings.HasPrefix(q, `\\`) || strings.Contains(q, `\`)
}

func isWindowsDriveQuery(q string) bool {
	if len(q) < 2 || q[1] != ':' {
		return false
	}
	c := q[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func lastPathSeparator(s string) int {
	slash := strings.LastIndexByte(s, '/')
	backslash := strings.LastIndexByte(s, '\\')
	if backslash > slash {
		return backslash
	}
	return slash
}

// candidateDirs unions the directory source's frecent dirs with every directory
// that already holds a session, newest sessions first.
func (a App) candidateDirs(cfg config.Config) []string {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		out = append(out, d)
	}
	for _, s := range session.Index(cfg) {
		add(s.Dir)
	}
	for _, d := range a.dirs.Candidates() {
		add(d)
	}
	return out
}

// indexedSessions returns the local index after applying cheap archive-only
// auto-retirement. It mutates only metadata sidecars for sessions that newly
// cross the retention threshold.
func indexedSessions(cfg config.Config) []session.Session {
	sessions := session.Index(cfg)
	if refreshReopenedTurns(cfg, sessions) {
		sessions = session.Index(cfg)
	}
	rt := state.ComputeAll(sessions)
	if _, err := retention.ApplyAuto(cfg.Retention, sessions, rt, time.Now()); err != nil {
		axlog.Printf("retention auto-retire: %v", err)
	}
	return sessions
}

// List prints every indexed local session, one rendered row per line. With
// --json it emits the structured self-report instead (the federation wire
// format). The default is local-only and fast; --federated (alias --hosts) opts
// into fanning out to every configured host and merging their sessions in,
// host-qualified.
func (a App) List(args []string) {
	group, args := GroupArg(args)
	var jsonOut, federated bool
	arch := retention.ActiveOnly
	for _, x := range args {
		switch x {
		case "--json":
			jsonOut = true
		case "--federated", "--hosts":
			federated = true
		case "--all":
			arch = retention.All
		case "--archived":
			arch = retention.ArchivedOnly
		}
	}
	if jsonOut {
		if federated {
			a.listFederatedJSON(group, arch)
		} else {
			encodeReport(a.localReport(group, arch)) // local-only self-report; never fans out
		}
		return
	}
	cfg, _ := config.Load()
	db := models.Load()
	sessions := indexedSessions(cfg)
	if group != "" {
		sessions = filterGroup(sessions, group)
	}
	sessions = retention.FilterSessions(sessions, arch)
	// Local rows adopt hand-started windows so they show live here too. Remote
	// rows carry their owner's reported runtime state, keyed by session.Key.
	locators := adopt.Locators(a.mux, sessions, harnessNames(cfg))
	var remoteState map[string]state.Runtime
	if federated {
		var remote []session.Session
		remote, remoteState = federatedHostsWithFilter(hostreg.Merge(cfg.Hosts), arch)
		if group != "" {
			remote = filterGroup(remote, group)
		}
		sessions = append(sessions, remote...)
	}
	cfg = composeCols(cfg, sessions, len(remoteState) > 0)
	meta := finder.BuildMeta(sessions, locators, remoteState)
	d := view.DefaultSortCol(cfg)
	view.Sort(cfg, sessions, db, d, view.DefaultDescFor(cfg, d), meta)
	for _, s := range sessions {
		fmt.Println(view.StripANSI(view.Row(cfg, db, s, meta[session.Key(s)], 0)))
	}
}

// Preview prints one session's rendered preview (metadata header plus recent
// turns). A remote ax serves this over the transport so a viewer's picker can
// lazily load a remote session's body on demand; see the RemotePreview closure
// in Pick and `ax preview` in usage.
func (a App) Preview(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "ax: preview needs a session id")
		os.Exit(1)
	}
	cfg, _ := config.Load()
	db := models.Load()
	sessions := session.Index(cfg)
	id := resolveIDFromSessions(args[0], sessions)
	for _, s := range sessions {
		if s.ID == id {
			fmt.Print(view.Preview(cfg, db, s))
			return
		}
	}
	fmt.Fprintln(os.Stderr, "ax: session not found:", args[0])
	os.Exit(1)
}

// SearchResult is one ranked match in `ax search --json`: the session's id and
// the control-layer metadata a reuse-vs-spawn decision needs (task, labels,
// project, state, context fill, recency, cost), plus the matched-line snippets
// and the hit count it is ranked by. It composes what used to take a search +
// `ax list --json` + a hand join into one payload.
// Ranking is dumb mechanism (count matches); any reuse threshold is recipe policy.
type SearchResult struct {
	ID        string    `json:"id"`
	Harness   string    `json:"harness"`
	Name      string    `json:"name,omitempty"`
	Task      string    `json:"task,omitempty"`
	Title     string    `json:"title,omitempty"`
	Project   string    `json:"project,omitempty"` // the "project" label, pulled out for convenience
	Group     string    `json:"group,omitempty"`
	Labels    []string  `json:"labels,omitempty"`
	State     string    `json:"state"` // done|failed|blocked|working|idle|crash|inactive (flattened runtime)
	Dir       string    `json:"dir,omitempty"`
	Model     string    `json:"model,omitempty"`
	CtxTok    int       `json:"ctx_tok"`    // context fill on the last turn
	CtxWindow int       `json:"ctx_window"` // model context window (0 when unknown)
	Last      time.Time `json:"last"`       // recency, for a cache-warmth judgment
	Cost      float64   `json:"cost,omitempty"`
	HasCost   bool      `json:"has_cost,omitempty"`
	Hits      int       `json:"hits"`               // match count, capped at searchHitCap; the ranking key
	Snippets  []string  `json:"snippets,omitempty"` // the first matching lines from the prose sidecar
}

// searchHitCap bounds how many matches per session the ranker counts and reads
// (a fat transcript can match a common term thousands of times; ranking needs a
// count, not every line). searchSnippetCap bounds how many matched lines are
// carried back as snippets.
const (
	searchHitCap     = 50
	searchSnippetCap = 5
)

// Search finds this host's sessions whose transcript contains query. Plain
// output is the backward-compatible bare id list (now in rank order); `--json`
// returns ranked SearchResults carrying per-session metadata and matched-line
// snippets, the evidence a caller uses to decide reuse vs spawn. A viewer's
// picker calls it over the transport (`ax search <query> --json`) to filter and
// order remote sessions.
func (a App) Search(args []string) {
	jsonOut := false
	var qp []string
	for _, x := range args {
		if x == "--json" {
			jsonOut = true
		} else {
			qp = append(qp, x)
		}
	}
	query := strings.Join(qp, " ")

	cfg, _ := config.Load()
	sessions := session.Index(cfg)
	results := a.searchResults(cfg, sessions, query)

	if jsonOut {
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.ID
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(struct {
			Results []SearchResult `json:"results"`
			// IDs is the ranked id list, kept for older consumers (and remoteSearch)
			// that only read ids over the transport.
			IDs []string `json:"ids"`
		}{results, ids})
		return
	}
	for _, r := range results {
		fmt.Println(r.ID)
	}
}

// searchResults runs the content search over every local session's prose sidecar
// and returns the matches ranked by hit count (recency breaks ties), each carried
// with its session metadata and snippet lines. It is the shared core of `ax
// search` (both output modes) and the remote-served search.
func (a App) searchResults(cfg config.Config, sessions []session.Session, query string) []SearchResult {
	if strings.TrimSpace(query) == "" {
		return []SearchResult{}
	}
	fileSess := map[string]session.Session{}
	var files []string
	for _, s := range sessions {
		if s.Host != "" {
			continue // a remote row's sidecar is not on this host; its owner searches it
		}
		if f := view.TextFile(cfg, s); f != "" {
			fileSess[f] = s
			files = append(files, f)
		}
	}
	rt := state.ComputeAll(sessions)
	matches := search.New(cfg).Matches(query, files, searchHitCap)
	results := make([]SearchResult, 0, len(matches))
	for f, lines := range matches {
		s, ok := fileSess[f]
		if !ok {
			continue
		}
		results = append(results, SearchResult{
			ID:        s.ID,
			Harness:   s.Harness,
			Name:      s.Name,
			Task:      s.Task,
			Title:     s.Title,
			Project:   session.LabelValue(s.Labels, "project"),
			Group:     s.Group,
			Labels:    s.Labels,
			State:     flatState(s, rt[s.ID]),
			Dir:       s.Dir,
			Model:     s.Model,
			CtxTok:    s.CtxTok,
			CtxWindow: s.CtxWindow,
			Last:      s.Last,
			Cost:      s.Cost,
			HasCost:   s.HasCost,
			Hits:      len(lines),
			Snippets:  snippetLines(f, lines, searchSnippetCap),
		})
	}
	// Rank: most hits first, then most recent, then id for a stable order.
	sort.Slice(results, func(i, j int) bool {
		if results[i].Hits != results[j].Hits {
			return results[i].Hits > results[j].Hits
		}
		if !results[i].Last.Equal(results[j].Last) {
			return results[i].Last.After(results[j].Last)
		}
		return results[i].ID < results[j].ID
	})
	return results
}

// flatState collapses a session's runtime and outcome into one word a
// caller can branch on: a concluded/failed marker wins over live activity,
// which wins over a bare heartbeat.
func flatState(s session.Session, r state.Runtime) string {
	switch {
	case r.Failed || s.Outcome == "failure":
		return "failed"
	case r.Done || s.Outcome == "success":
		return "done"
	case r.Waiting == "input":
		return "blocked"
	case r.Waiting == "children":
		return "working"
	case r.Activity == state.Working:
		return "working"
	case r.State == state.Live:
		return "idle"
	case r.State == state.Crash:
		return "crash"
	default:
		return "inactive"
	}
}

// snippetLines reads up to limit of the matched lines out of a prose sidecar and
// returns them trimmed and length-bounded, so a JSON result carries readable
// context for why a session matched without dumping whole turns.
func snippetLines(file string, lines []int, limit int) []string {
	if len(lines) == 0 || limit == 0 {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	all := strings.Split(string(data), "\n")
	out := make([]string, 0, limit)
	for _, n := range lines {
		if n < 1 || n > len(all) {
			continue
		}
		s := strings.TrimSpace(all[n-1])
		if s == "" {
			continue
		}
		if len(s) > 200 {
			s = s[:200]
		}
		out = append(out, s)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// remoteSearch runs `ax search <query> --json` on each online host over its
// transport and returns the set of matching remote session keys ("host/id"). The
// picker calls it off the UI goroutine, debounced, so ssh latency stays off the
// keystroke path.
func (a App) remoteSearch(hostByName map[string]config.Host, hosts []view.HostStatus, query string) map[string]bool {
	// Hosts fan out in parallel (same pattern as federatedSessions): total
	// latency is the slowest host, not the sum, and one dead host costs its
	// timeout without delaying the others.
	type hit struct{ keys []string }
	results := make(chan hit, len(hosts))
	n := 0
	for _, hs := range hosts {
		if hs.State != view.HostOnline {
			continue
		}
		h, ok := hostByName[hs.Name]
		if !ok {
			continue
		}
		n++
		go func(name string, h config.Host) {
			prog, argv := transportArgv(h, "search", query, "--json")
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			res, err := exec.CommandContext(ctx, prog, argv...).Output()
			if err != nil {
				axlog.Printf("search on %s: %v", name, err)
				results <- hit{}
				return
			}
			var payload struct {
				IDs []string `json:"ids"`
			}
			var keys []string
			if json.Unmarshal(res, &payload) == nil {
				for _, id := range payload.IDs {
					keys = append(keys, name+"/"+id)
				}
			}
			results <- hit{keys: keys}
		}(hs.Name, h)
	}
	out := map[string]bool{}
	for ; n > 0; n-- {
		for _, k := range (<-results).keys {
			out[k] = true
		}
	}
	return out
}

// localReport builds this host's self-report from its own index alone: it reads
// no hosts and can never recurse, so a remote's `ax list --json` (which is what
// fetchHost invokes) stays local-only no matter what the caller passed. The
// runtime state, including blocked-on-a-human and owner wait markers, is
// computed here on the owner.
func (a App) localReport(group string, arch retention.ArchiveFilter) wire.Report {
	cfg, _ := config.Load()
	sessions := indexedSessions(cfg)
	if group != "" {
		sessions = filterGroup(sessions, group)
	}
	sessions = retention.FilterSessions(sessions, arch)
	rt := state.ComputeAll(sessions)
	pending := ask.List() // blocked-on-a-human is owner-computed state; federate it
	outcome := map[string]string{}
	for _, s := range sessions {
		outcome[s.ID] = s.Outcome
	}
	for id := range pending {
		if r, ok := rt[id]; ok {
			switch {
			case outcome[id] == "success":
				r.Waiting = "done" // presenting a result, not stuck
			default:
				// A pending ax ask is blocked on input until a real terminal
				// Done/Failed marker arrives; never synthesize Done from it.
				r.Waiting = "input"
			}
			rt[id] = r
		}
	}
	host, _ := os.Hostname()
	rep := wire.Report{
		SchemaVersion: wire.SchemaVersion,
		Hostname:      host,
		GeneratedAt:   time.Now(),
		Sessions:      make([]wire.Session, len(sessions)),
		Capability:    capabilityReport(cfg),
	}
	for i, s := range sessions {
		rep.Sessions[i] = toWire(s, rt[s.ID])
	}
	return rep
}

// listFederatedJSON is the opt-in `ax list --federated --json`: the local
// self-report with every configured host's sessions merged in, each remote row
// host-qualified (session.Key, i.e. "host/id"). The fan-out lives only here on
// the CLI caller, so the per-host self-reports it reads never fan out and this
// does not recurse. An offline/no-ax host is noted and contributes nothing.
func (a App) listFederatedJSON(group string, arch retention.ArchiveFilter) {
	rep := a.localReport(group, arch)
	cfg, _ := config.Load()
	remote, rrt := federatedHostsWithFilter(hostreg.Merge(cfg.Hosts), arch)
	if group != "" {
		remote = filterGroup(remote, group)
	}
	for _, s := range remote {
		ws := toWire(s, rrt[session.Key(s)])
		ws.ID = session.Key(s) // host-qualify the remote row's id
		rep.Sessions = append(rep.Sessions, ws)
	}
	encodeReport(rep)
}

func encodeReport(rep wire.Report) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
}

// keepLiveActive reports whether a keep-live exemption is in force now: an
// indefinite --keep-live (zero deadline) or an unexpired --keep-live-for lease.
// A lease whose deadline has passed is no longer active, so the worker is
// reapable again.
func keepLiveActive(keepLive bool, keepUntil, now time.Time) bool {
	if !keepLive {
		return false
	}
	return keepUntil.IsZero() || now.Before(keepUntil)
}

// reuseReadyFacts is the single mechanical predicate behind both the reuse_ready
// list fact and `ax continue`'s live-reuse accept, so the two can never drift: a
// live, task-concluded interactive worker that is not failed/waiting/working and
// whose keep-live is currently in force. It is a fact ("continue would accept
// this now"), not a recommendation.
func reuseReadyFacts(r state.Runtime, mode, task string, keepLive bool, keepUntil, now time.Time) bool {
	return r.State == state.Live && r.Done && !r.Failed && r.Waiting == "" && r.Activity != state.Working &&
		mode == "interactive" && strings.TrimSpace(task) != "" &&
		keepLiveActive(keepLive, keepUntil, now)
}

func toWire(s session.Session, r state.Runtime) wire.Session {
	return wire.Session{
		Harness:     s.Harness,
		ID:          s.ID,
		Dir:         s.Dir,
		Model:       s.Model,
		Title:       s.Title,
		Last:        s.Last,
		InTok:       s.InTok,
		OutTok:      s.OutTok,
		CacheReadT:  s.CacheReadT,
		CacheWriteT: s.CacheWriteT,
		CtxTok:      s.CtxTok,
		CtxWindow:   s.CtxWindow,
		Cost:        s.Cost,
		HasCost:     s.HasCost,
		Name:        s.Name,
		Task:        s.Task,
		Group:       s.Group,
		Parent:      s.Parent,
		Origin:      s.Origin,
		Mode:        s.Mode,
		Labels:      s.Labels,
		State:       r.State,
		Activity:    r.Activity,
		Lifecycle:   state.Lifecycle(r),
		Archived:    s.Archived,
		ArchivedAt:  s.ArchivedAt,
		Ephemeral:   state.Ephemeral(s.Parent),
		DirExists:   r.DirExists,
		Yolo:        r.Yolo,
		Waiting:     r.Waiting,
		Done:        r.Done,
		Restartable: s.HasSpec,
		Failed:      r.Failed,
		FailReason:  s.FailReason,
		KeepLive:    s.KeepLive,
		KeepUntil:   s.KeepUntil,
		ReuseReady:  reuseReadyFacts(r, s.Mode, s.Task, s.KeepLive, s.KeepUntil, time.Now()),
		TerminalAt:  r.TerminalAt,
		IdleSince:   s.Last,
	}
}

// fanoutTimeout bounds the wait on each host's `ax list --json`. The fan-out
// streams into the open picker (see View.HostUpdates), so this never sits on
// the interactive path; it only bounds how long a dead host's roster entry
// says "pending" before flipping to offline. (A last-known-good cache comes
// later.)
const fanoutTimeout = 3 * time.Second

// federation tracks the streamed host fan-out behind an open picker. Each
// configured host's fetch result is recorded here as it completes, and every
// consumer (the per-host snapshot updates, the reindex tick, remote search)
// reads the merged state so far instead of blocking on the slowest host.
type federation struct {
	mu    sync.Mutex
	hosts []config.Host         // config order, so the roster is stable
	got   map[string]hostResult // completed fetches by host name
}

// hostResult is one host's fetch outcome: its sessions and their
// owner-reported runtime state on success, and its federation state always.
type hostResult struct {
	sessions []session.Session
	rt       map[string]state.Runtime
	state    string
}

func newFederation(hosts []config.Host) *federation {
	return &federation{hosts: hosts, got: map[string]hostResult{}}
}

// record stores one host's completed fetch.
func (f *federation) record(name string, r hostResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got[name] = r
}

func (f *federation) setArchived(host, id string, archived bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.got[host]
	if !ok {
		return
	}
	now := time.Now()
	for i := range r.sessions {
		if r.sessions[i].ID != id {
			continue
		}
		r.sessions[i].Archived = archived
		if archived {
			r.sessions[i].ArchivedAt = now
		} else {
			r.sessions[i].ArchivedAt = time.Time{}
		}
	}
	f.got[host] = r
}

// merged returns the sessions and runtime state of every host that has
// answered online so far. Offline/no-ax/stale hosts contribute nothing; a
// still-pending host simply isn't in the set yet.
func (f *federation) merged() ([]session.Session, map[string]state.Runtime) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sessions []session.Session
	rt := map[string]state.Runtime{}
	for _, h := range f.hosts {
		r, ok := f.got[h.Name]
		if !ok || r.state != view.HostOnline {
			continue
		}
		sessions = append(sessions, r.sessions...)
		for id, st := range r.rt {
			rt[id] = st
		}
	}
	return sessions, rt
}

// roster is the machine list for the picker's status line: local first, then
// each configured host with its current federation state (pending until its
// fetch returns).
func (f *federation) roster(localCount int) []view.HostStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	hosts := []view.HostStatus{{Name: "local", State: view.HostLocal, Sessions: localCount}}
	for _, h := range f.hosts {
		r, ok := f.got[h.Name]
		if !ok {
			hosts = append(hosts, view.HostStatus{Name: h.Name, State: view.HostPending})
			continue
		}
		hosts = append(hosts, view.HostStatus{Name: h.Name, State: r.state, Sessions: len(r.sessions)})
	}
	return hosts
}

// fedSnapshot builds the picker's full current view: a fresh local scan (the
// index is mtime-cached, so this is milliseconds) plus every host result
// received so far, with the column layout recomposed. Shared by the reindex
// tick and the streamed host updates so both merge paths deliver the same
// shape. fallback is the pre-composition config to keep when a mid-edit
// config fails to parse. fed may be nil (no hosts configured).
func (a App) fedSnapshot(fallback config.Config, fed *federation) finder.HostUpdate {
	ncfg, err := config.Load()
	if err != nil {
		ncfg = fallback // a mid-edit unparseable config: keep the last good one
	} else {
		ncfg.Hosts = hostreg.Merge(ncfg.Hosts)
	}
	local := indexedSessions(ncfg)
	all := local
	rt := map[string]state.Runtime{}
	var roster []view.HostStatus
	if fed != nil {
		var remote []session.Session
		remote, rt = fed.merged()
		all = append(all, remote...)
		roster = fed.roster(len(local))
	}
	return finder.HostUpdate{
		Sessions:    all,
		Config:      composeCols(ncfg, all, len(rt) > 0),
		RemoteState: rt,
		Hosts:       roster,
	}
}

// transportArgv splits a host's transport into the program and its args, then
// appends `<ax> <axArgs...>`. So a transport "ssh -t box" and axArgs
// ["list","--json"] runs ssh with args ["-t","box","ax","list","--json"]. ax
// never opens a socket of its own; it rides the transport's own authentication.
func transportArgv(h config.Host, axArgs ...string) (string, []string) {
	fields := strings.Fields(h.Transport)
	if len(fields) == 0 {
		return "", nil
	}
	ax := h.Ax
	if ax == "" {
		ax = "ax"
	}
	args := axArgs
	if !h.RawArgv {
		// An ssh-style transport joins argv into one string the remote shell
		// re-parses, so every value is quoted for that remote shell, not the local
		// platform's. The quoting differs by shell: POSIX sh (the default) honors
		// the '\'' embedded-quote escape, but PowerShell does not and escapes an
		// embedded quote only by doubling it, so a pwsh host (shell = "pwsh") must
		// be quoted with QuotePwsh. Quoting a pwsh remote as POSIX would flip quote
		// parity on any embedded quote, letting a crafted value inject commands. A
		// raw transport (kubectl exec ... --, docker exec) passes argv verbatim,
		// where quotes would arrive as literal characters; raw_argv = true turns
		// this off.
		quote := shell.QuotePosix
		if h.Shell == "pwsh" {
			quote = shell.QuotePwsh
		} else {
			// Empty shell over an ssh transport means POSIX quoting by default,
			// which mis-quotes on a PowerShell host; warn once so the operator
			// can set shell = "pwsh" if this host is actually Windows.
			warnMissingShell(h)
		}
		args = make([]string, len(axArgs))
		for i, a := range axArgs {
			args[i] = quote(a)
		}
	}
	argv := append(append([]string{}, fields[1:]...), append([]string{ax}, args...)...)
	return fields[0], argv
}

// fetchHostFn is the seam the CLI fan-out calls through, so a test can inject
// canned host results without an ssh round-trip. Production points it at
// fetchHost.
var fetchHostFn = fetchHost

func federatedHostsWithFilter(hosts []config.Host, arch retention.ArchiveFilter) ([]session.Session, map[string]state.Runtime) {
	if len(hosts) == 0 {
		return nil, nil
	}
	fed := newFederation(hosts)
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(h config.Host) {
			defer wg.Done()
			ss, rt, st := fetchHostFn(h)
			if st != view.HostOnline {
				fmt.Fprintf(os.Stderr, "ax: host %s: %s\n", h.Name, st)
			}
			fed.record(h.Name, hostResult{sessions: ss, rt: rt, state: st})
		}(h)
	}
	wg.Wait()
	ss, rt := fed.merged()
	ss = retention.FilterSessions(ss, arch)
	frt := make(map[string]state.Runtime, len(ss))
	for _, s := range ss {
		k := session.Key(s)
		if r, ok := rt[k]; ok {
			frt[k] = r
		}
	}
	return ss, frt
}

// fetchHost runs `<transport> <ax> list --json --all` on a host and converts its report
// into sessions (host label set) plus their reported runtime state. The third
// return is the host's state: online on success, or the reason it failed
// (offline for an unreachable/timed-out transport, no-ax when ssh works but ax
// isn't there, old-ax on a version mismatch).
func fetchHost(h config.Host) ([]session.Session, map[string]state.Runtime, string) {
	if strings.TrimSpace(h.Transport) == "" {
		return nil, nil, view.HostOffline
	}
	prog, argv := transportArgv(h, "list", "--json", "--all")
	ctx, cancel := context.WithTimeout(context.Background(), fanoutTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, prog, argv...).Output()
	if err != nil {
		var ee *exec.ExitError
		if ctx.Err() != context.DeadlineExceeded && errors.As(err, &ee) && ee.ExitCode() == 127 {
			return nil, nil, view.HostNoAx // remote shell: "ax: command not found"
		}
		return nil, nil, view.HostOffline
	}
	var rep wire.Report
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, nil, view.HostOffline
	}
	if rep.SchemaVersion < wire.MinSchemaVersion || rep.SchemaVersion > wire.SchemaVersion {
		return nil, nil, view.HostStale
	}
	sessions := make([]session.Session, len(rep.Sessions))
	rt := make(map[string]state.Runtime, len(rep.Sessions))
	for i, ws := range rep.Sessions {
		sessions[i] = fromWire(ws, h.Name)
		// Key by session.Key (host/id) so the override in BuildMeta cannot collide
		// with a local session that happens to share the bare id.
		rt[session.Key(sessions[i])] = state.Runtime{State: ws.State, Activity: ws.Activity, DirExists: ws.DirExists, Yolo: ws.Yolo, Waiting: ws.Waiting, Done: ws.Done, Failed: ws.Failed, TerminalAt: ws.TerminalAt}
	}
	return sessions, rt, view.HostOnline
}

// fromWire converts a received wire.Session into the internal session shape,
// tagging it with the host it came from. Its runtime state travels separately.
func fromWire(ws wire.Session, host string) session.Session {
	return session.Session{
		Harness:     ws.Harness,
		Host:        host,
		ID:          ws.ID,
		Dir:         ws.Dir,
		Model:       ws.Model,
		Title:       ws.Title,
		Last:        ws.Last,
		InTok:       ws.InTok,
		OutTok:      ws.OutTok,
		CacheReadT:  ws.CacheReadT,
		CacheWriteT: ws.CacheWriteT,
		CtxTok:      ws.CtxTok,
		CtxWindow:   ws.CtxWindow,
		Cost:        ws.Cost,
		HasCost:     ws.HasCost,
		Name:        ws.Name,
		Task:        ws.Task,
		Group:       ws.Group,
		Parent:      ws.Parent,
		Origin:      ws.Origin,
		Mode:        ws.Mode,
		Labels:      ws.Labels,
		HasSpec:     ws.Restartable,
		FailReason:  ws.FailReason,
		KeepLive:    ws.KeepLive,
		KeepUntil:   ws.KeepUntil,
		Archived:    ws.Archived,
		ArchivedAt:  ws.ArchivedAt,
	}
}

// composeCols augments the layout with the auto columns each session set earns:
// HOST when federating, NAME/ID for runs, TAGS for labels, ⚠ for an unguarded
// agent. Shared by the initial open and the picker's live reindex, so a session
// that first introduces one of these makes its column appear on the next scan.
func composeCols(cfg config.Config, sessions []session.Session, hasRemote bool) config.Config {
	cfg = view.WithLifecycleColumn(cfg)
	if hasRemote {
		cfg = view.WithHostColumn(cfg)
	}
	if anyGrouped(sessions) {
		cfg = view.WithGroupColumns(cfg)
	}
	if anyLabeled(sessions) {
		cfg = view.WithTagsColumn(cfg)
	}
	if anyYolo() {
		cfg = view.WithYoloColumn(cfg)
	}
	return cfg
}

// anyLabeled reports whether any session carries labels, so the TAGS column only
// appears once something is actually tagged.
func anyLabeled(sessions []session.Session) bool {
	for _, s := range sessions {
		if len(s.Labels) > 0 {
			return true
		}
	}
	return false
}

// anyYolo reports whether any live session runs without guardrails, so the picker
// surfaces the ⚠ column only when there is an unsandboxed agent to warn about.
func anyYolo() bool {
	for _, e := range live.Snapshot() {
		if live.Running(e) && state.IsYolo(e.Cmd) {
			return true
		}
	}
	return false
}

// live returns the session->locator map when inside the multiplexer, else nil.
func (a App) live() map[string]string {
	if a.mux.Active() {
		return a.mux.Live()
	}
	return nil
}

// mergeAdopted folds adopt's window matches into a locator map for BuildMeta.
func (a App) mergeAdopted(loc map[string]string, adopted map[string]mux.Pane) map[string]string {
	if len(adopted) == 0 {
		return loc
	}
	if loc == nil {
		loc = map[string]string{}
	}
	for k, p := range adopted {
		loc[k] = p.Locator
	}
	return loc
}

// harnessNames is the set of configured harness names, for telling a harness pane
// from a plain shell during adopt correlation.
func harnessNames(cfg config.Config) map[string]bool {
	m := make(map[string]bool, len(cfg.Harnesses))
	for _, h := range cfg.Harnesses {
		m[h.Name] = true
	}
	return m
}

// UpdateModels refreshes the model price/context snapshot from models.dev.
func UpdateModels(cfg config.Config, args []string) {
	if len(args) == 0 || args[0] != "update" {
		fmt.Fprintln(os.Stderr, "usage: ax models update")
		os.Exit(2)
	}
	if cfg.Offline || os.Getenv("AX_OFFLINE") != "" {
		fmt.Fprintln(os.Stderr, "ax: offline mode is on; models.dev network call skipped (unset offline / AX_OFFLINE to update)")
		os.Exit(1)
	}
	n, err := models.Update()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ax: models update failed:", err)
		os.Exit(1)
	}
	fmt.Printf("updated %d models from models.dev\n", n)
}

// resumeCmd fills a harness's resume template for a session. override, when
// non-nil, replaces the harness's default Args for this launch (an empty string
// clears them). It first moves the transcript to where the harness will look
// for it in s.Dir (a session stranded by a directory move would otherwise
// resume into "No conversation found").
func resumeCmd(h config.Harness, s session.Session, override *string) string {
	if err := session.Relocate(h, s); err != nil {
		axlog.Printf("relocate %s: %v", s.ID, err)
	}
	// A placeholder model (claude logs "<synthetic>" on harness-injected
	// messages) is not resumable; drop it so the --model flag falls away and the
	// harness picks its default. Old index caches can still carry one.
	model := s.Model
	if strings.HasPrefix(model, "<") {
		model = ""
	}
	// dir/id/model come from the transcript (untrusted); fillTemplate quotes them
	// so a working directory or model name with shell metacharacters can't run code
	// on resume. {args} stays shell-splittable (user config).
	return fillTemplate(h.Resume, map[string]string{
		"id": s.ID, "dir": s.Dir, "model": model, "args": argsFor(h, override),
	})
}

// launchCmd fills a harness's launch template for a fresh session, substituting
// a minted session id into the {newid} slot (empty when the harness has none).
// {behavior} and {task} are empty here (the interactive path), so they drop.
func launchCmd(h config.Harness, override *string, newid string) string {
	return fillTemplate(h.Launch, map[string]string{
		"newid": newid, "args": argsFor(h, override),
	})
}

// fillTemplate substitutes a harness command template's placeholders. The
// untrusted / free-text values (id, dir, model, newid, behavior, task) are
// shell-quoted; {args} stays splittable (fillArgs). A flag-guarded optional value
// (model, behavior) drops together with its flag when empty (so "--model {model}"
// and "--append-system-prompt {behavior}" vanish); a bare positional (task) drops
// its own slot; id/dir/newid collapse to nothing in place, preserving surrounding
// text like "cd {dir} &&".
func fillTemplate(tmpl string, vals map[string]string) string {
	for _, k := range []string{"model", "behavior"} { // flag-guarded optionals
		ph := "{" + k + "}"
		if vals[k] == "" {
			tmpl = dropFlagged(tmpl, ph)
		} else {
			tmpl = strings.ReplaceAll(tmpl, ph, quoteVal(vals[k]))
		}
	}
	switch {
	case vals["task"] == "": // bare positional
		tmpl = strings.ReplaceAll(strings.ReplaceAll(tmpl, " {task}", ""), "{task}", "")
	case strings.Contains(tmpl, "{task}"):
		tmpl = strings.ReplaceAll(tmpl, "{task}", quoteVal(vals["task"]))
	default:
		// A template with no {task} slot (a pre-control-layer user override) still
		// gets the prompt appended, like {args}: dropping it would launch a worker
		// with no task at all.
		tmpl += " " + quoteVal(vals["task"])
	}
	tmpl = strings.NewReplacer(
		"{id}", quoteVal(vals["id"]),
		"{dir}", quoteVal(vals["dir"]),
		"{newid}", quoteVal(vals["newid"]),
	).Replace(tmpl)
	return fillArgs(tmpl, vals["args"])
}

// dropFlagged removes " {ph}" and, when the token immediately before it is a flag
// (starts with "-"), that flag too, so an unset flag-guarded placeholder leaves no
// dangling flag. Only the first occurrence is dropped (a placeholder appears once).
func dropFlagged(tmpl, ph string) string {
	i := strings.Index(tmpl, ph)
	if i < 0 {
		return tmpl
	}
	end := i + len(ph)
	start := i
	if start > 0 && tmpl[start-1] == ' ' {
		start-- // the space before {ph}
	}
	j := start
	for j > 0 && tmpl[j-1] != ' ' {
		j-- // start of the token before {ph}
	}
	if strings.HasPrefix(tmpl[j:start], "-") {
		start = j
		if start > 0 && tmpl[start-1] == ' ' {
			start--
		}
	}
	return tmpl[:start] + tmpl[end:]
}

// newUUID returns a random v4 UUID (the session-id shape every harness uses).
// On the (vanishingly unlikely) failure to read randomness it returns "", which
// makes the caller fall back to the untracked-launch path.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func argsFor(h config.Harness, override *string) string {
	if override != nil {
		return *override
	}
	return h.Args
}

// fillArgs substitutes the {args} slot, dropping it cleanly (with any leading
// space) when there are no args. Templates without the slot get the args
// appended, so a user config predating {args} still injects them.
func fillArgs(tmpl, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		tmpl = strings.ReplaceAll(tmpl, " {args}", "")
		return strings.ReplaceAll(tmpl, "{args}", "")
	}
	if strings.Contains(tmpl, "{args}") {
		return strings.ReplaceAll(tmpl, "{args}", args)
	}
	return tmpl + " " + args
}
