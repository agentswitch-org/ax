package app

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/retention"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
)

var errArchiveLive = errors.New("live session requires --force")

// Archive hides sessions from default views without deleting any data.
func (a App) Archive(args []string) {
	if host, rest := dehost(args); host != "" {
		a.remoteVerb("archive", host, rest, false)
		return
	}
	force := false
	var ids []string
	for _, arg := range args {
		switch arg {
		case "--force":
			force = true
		default:
			if strings.HasPrefix(arg, "-") {
				fmt.Fprintf(os.Stderr, "ax archive: unknown flag %q\n", arg)
				os.Exit(2)
			}
			ids = append(ids, arg)
		}
	}
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "ax archive: need one or more session ids")
		os.Exit(2)
	}
	cfg, _ := config.Load()
	sessions := session.Index(cfg)
	byID := sessionsByID(sessions)
	liveIDs := live.LiveIDs()
	var failed int
	for _, raw := range ids {
		id := resolveID(raw)
		if err := archiveSession(id, force, liveIDs); errors.Is(err, errArchiveLive) {
			fmt.Fprintf(os.Stderr, "ax archive: %s is live (use --force to archive anyway)\n", id)
			failed++
			continue
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "ax archive: %s: %v\n", id, err)
			failed++
			continue
		}
		if s, ok := byID[id]; ok && s.Archived {
			fmt.Printf("already archived %s\n", id)
		} else {
			fmt.Printf("archived %s\n", id)
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func archiveSession(id string, force bool, liveIDs map[string]bool) error {
	if liveIDs[id] && !force {
		return errArchiveLive
	}
	return meta.SetArchived(id, true)
}

// Unarchive restores sessions to default views.
func (a App) Unarchive(args []string) {
	if host, rest := dehost(args); host != "" {
		a.remoteVerb("unarchive", host, rest, false)
		return
	}
	var ids []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			fmt.Fprintf(os.Stderr, "ax unarchive: unknown flag %q\n", arg)
			os.Exit(2)
		}
		ids = append(ids, arg)
	}
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "ax unarchive: need one or more session ids")
		os.Exit(2)
	}
	var failed int
	for _, raw := range ids {
		id := resolveID(raw)
		if err := meta.SetArchived(id, false); err != nil {
			fmt.Fprintf(os.Stderr, "ax unarchive: %s: %v\n", id, err)
			failed++
			continue
		}
		fmt.Printf("unarchived %s\n", id)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// Prune archives safe local lifecycle clutter and, with --reap-workers, closes
// concluded resident worker processes/windows. It never deletes transcripts or
// metadata and it never archives durable, live, or pending-ask sessions.
func (a App) Prune(args []string) {
	run := ""
	host := ""
	dryRun := false
	reapWorkers := false
	var olderThan time.Duration
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			dryRun = true
		case "--reap-workers":
			reapWorkers = true
		case "--all":
		case "--run", "--group":
			run = nextVal(args[i], args, &i)
		case "--host":
			host = nextVal("--host", args, &i)
		case "--older-than":
			d, err := time.ParseDuration(nextVal("--older-than", args, &i))
			if err != nil {
				fmt.Fprintf(os.Stderr, "ax prune: invalid --older-than: %v\n", err)
				os.Exit(2)
			}
			olderThan = d
		default:
			fmt.Fprintf(os.Stderr, "ax prune: unknown flag %q\n", args[i])
			os.Exit(2)
		}
	}
	if host != "" && !isLocalHost(host) {
		fmt.Fprintf(os.Stderr, "ax prune: remote prune is local-only in this build; run prune on host %s\n", host)
		os.Exit(2)
	}
	cfg, _ := config.Load()
	var sessions []session.Session
	if dryRun {
		sessions = session.IndexReadOnly(cfg)
	} else {
		sessions = session.Index(cfg)
	}
	rt := state.ComputeAll(sessions)
	cands := retention.PruneCandidates(cfg.Retention, sessions, rt, run, olderThan, time.Now())
	reapCands, reapSkips := a.workerReapCandidates(cfg, sessions, rt, run, olderThan, time.Now(), reapWorkers)
	var archived, failed int
	for _, c := range cands {
		id := c.Session.ID
		prefix := "archived"
		if dryRun {
			prefix = "would archive"
		}
		fmt.Printf("%s %s\t%s\t%s\n", prefix, id, c.Lifecycle, c.Reason)
		if dryRun {
			continue
		}
		if err := meta.SetArchived(id, true); err != nil {
			fmt.Fprintf(os.Stderr, "ax prune: %s: %v\n", id, err)
			failed++
			continue
		}
		archived++
	}
	var reaped int
	for _, c := range reapCands {
		prefix := "reaped worker"
		if dryRun {
			prefix = "would reap worker"
		}
		fmt.Printf("%s %s\t%s\t%s\n", prefix, c.Session.ID, c.Lifecycle, c.Reason)
		if dryRun {
			continue
		}
		if c.Locator != "" && muxHasWindows(a.mux) {
			if err := a.mux.CloseWindow(c.Session.ID); err != nil {
				axlog.Printf("prune reap %s: close window: %v", c.Session.ID, err)
			}
		}
		var err error
		if c.HasAdoptedWrapper {
			err = killAdoptedWrapperFn(c.AdoptedWrapper)
		} else {
			err = workerReapKillFn(c.Session.ID)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "ax prune: reap %s: %v\n", c.Session.ID, err)
			failed++
			continue
		}
		reaped++
	}
	if dryRun && reapWorkers {
		for _, s := range reapSkips {
			fmt.Printf("would skip worker %s\t%s\t%s\n", s.Session.ID, s.Lifecycle, s.Reason)
		}
	}
	if dryRun {
		if reapWorkers {
			fmt.Printf("prune: candidates=%d archived=0 reap_candidates=%d reaped=0 dry-run=true\n", len(cands), len(reapCands))
		} else {
			fmt.Printf("prune: candidates=%d archived=0 dry-run=true\n", len(cands))
		}
	} else {
		if reapWorkers {
			fmt.Printf("prune: candidates=%d archived=%d reap_candidates=%d reaped=%d failed=%d\n", len(cands), archived, len(reapCands), reaped, failed)
		} else {
			fmt.Printf("prune: candidates=%d archived=%d failed=%d\n", len(cands), archived, failed)
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}

type workerReapCandidate struct {
	Session           session.Session
	Lifecycle         string
	Reason            string
	Locator           string
	AdoptedWrapper    adoptedWrapper
	HasAdoptedWrapper bool
}

type workerReapSkip struct {
	Session   session.Session
	Lifecycle string
	Reason    string
}

func (a App) workerReapCandidates(cfg config.Config, sessions []session.Session, rt map[string]state.Runtime, run string, olderThan time.Duration, now time.Time, enabled bool) ([]workerReapCandidate, []workerReapSkip) {
	if !enabled {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	pending := ask.List()
	loc := map[string]string{}
	if muxHasWindows(a.mux) {
		loc = a.mux.Live()
	}
	var out []workerReapCandidate
	var skips []workerReapSkip
	current := os.Getenv("AX_SESSION_ID")
	orphans := adoptedWorkerOrphans()
	seen := map[string]bool{}
	for _, s := range sessions {
		if s.Host != "" {
			continue
		}
		seen[s.ID] = true
		if current != "" && s.ID == current {
			continue
		}
		if run != "" && s.Group != run {
			continue
		}
		if !s.Last.IsZero() && olderThan > 0 && s.Last.After(now.Add(-olderThan)) {
			continue
		}
		r := rt[s.ID]
		orphan, hasOrphan := orphans[s.ID]
		hasLiveResident := r.State == state.Live
		if hasOrphan && !hasLiveResident {
			r = adoptedWrapperRuntime(s.ID, sessions, rt)
			hasLiveResident = true
		}
		window := loc[s.ID]
		if !hasLiveResident && window == "" {
			continue
		}
		safeRuntime := r
		fileActivityVeto := false
		if window != "" && safeRuntime.State != state.Live {
			// Match picker.BuildMeta: an open mux window is an active access point
			// even without a fresh heartbeat, and its spinner comes from the
			// transcript/hook file activity signal.
			safeRuntime.State = state.Live
			safeRuntime.Activity = state.FileActivity(s)
			fileActivityVeto = muxWindowFileActivityVeto(s, safeRuntime, now)
		}
		_, pendingAsk := pending[s.ID]
		m := meta.Load(s.ID)
		if hooklessTurnStartedAfterTerminal(s.ID, cfg, sessions) {
			skips = append(skips, workerReapSkip{
				Session:   s,
				Lifecycle: state.Lifecycle(safeRuntime),
				Reason:    "newer hookless turn after terminal marker",
			})
			continue
		}
		if (safeRuntime.Activity == state.Working || fileActivityVeto) && r.Activity != state.Working && workerReapSafeIfIdle(s.ID, m, cfg.Retention, safeRuntime, pendingAsk) {
			skips = append(skips, workerReapSkip{
				Session:   s,
				Lifecycle: state.Lifecycle(safeRuntime),
				Reason:    "recent or newer transcript/file activity in mux window",
			})
			continue
		}
		if !workerReapSafeNow(s.ID, m, cfg.Retention, safeRuntime, pendingAsk) {
			continue
		}
		reason := "concluded worker"
		switch {
		case hasLiveResident && window != "":
			reason = "concluded worker with live process and mux window"
		case hasOrphan:
			reason = "concluded worker with adopted wrapper"
		case hasLiveResident:
			reason = "concluded worker with live process"
		case window != "":
			reason = "concluded worker with lingering mux window"
		}
		c := workerReapCandidate{Session: s, Lifecycle: state.Lifecycle(r), Reason: reason, Locator: window}
		if hasOrphan {
			c.AdoptedWrapper = orphan
			c.HasAdoptedWrapper = true
		}
		out = append(out, c)
	}
	for id, orphan := range orphans {
		if seen[id] {
			continue
		}
		if current != "" && id == current {
			continue
		}
		m := meta.Load(id)
		s := syntheticAdoptedWorkerSession(id, m)
		if run != "" && s.Group != run {
			continue
		}
		if !s.Last.IsZero() && olderThan > 0 && s.Last.After(now.Add(-olderThan)) {
			continue
		}
		if hooklessTurnStartedAfterTerminal(id, cfg, sessions) {
			skips = append(skips, workerReapSkip{
				Session:   s,
				Lifecycle: state.Lifecycle(adoptedWrapperRuntime(id, sessions, rt)),
				Reason:    "newer hookless turn after terminal marker",
			})
			continue
		}
		r := adoptedWrapperRuntime(id, sessions, rt)
		if !workerReapSafeNow(id, m, cfg.Retention, r, pendingAsk(id)) {
			continue
		}
		out = append(out, workerReapCandidate{
			Session:           s,
			Lifecycle:         state.Lifecycle(r),
			Reason:            "concluded worker with adopted wrapper",
			AdoptedWrapper:    orphan,
			HasAdoptedWrapper: true,
		})
	}
	return out, skips
}

func muxWindowFileActivityVeto(s session.Session, r state.Runtime, now time.Time) bool {
	if s.File == "" {
		return false
	}
	info, err := os.Stat(s.File)
	if err != nil {
		return false
	}
	mtime := info.ModTime()
	if mtime.IsZero() {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	if mtime.After(now.Add(-live.Active)) {
		return true
	}
	return !r.TerminalAt.IsZero() && !r.TerminalAt.Before(now.Add(-state.HookFresh)) && mtime.After(r.TerminalAt)
}

func workerReapSafeIfIdle(id string, m meta.Meta, ret config.Retention, r state.Runtime, pendingAsk bool) bool {
	idle := r
	idle.Activity = state.Idle
	return workerReapSafeNow(id, m, ret, idle, pendingAsk)
}

func sessionsByID(sessions []session.Session) map[string]session.Session {
	out := make(map[string]session.Session, len(sessions))
	for _, s := range sessions {
		out[s.ID] = s
	}
	return out
}

func isLocalHost(name string) bool {
	if name == "" || name == "local" {
		return true
	}
	host, _ := os.Hostname()
	return name == host
}
