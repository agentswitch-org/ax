package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/state"
)

// Result prints a concluded session's captured final report (its last assistant
// message, snapshotted at conclude time) plus its outcome and exit code: the
// interactive equivalent of the final answer a headless `claude -p` prints to
// stdout. The report goes to stdout (the payload); the outcome/exit go to stderr
// as a status line, so a caller can `ax result <id> > answer.txt` cleanly. With
// --json it emits {id, outcome, exit, result} as one object on stdout instead.
//
//	ax result <id> [--json]
func (a App) Result(args []string) {
	// A host-qualified id (host/id) or --host reruns the result on that host, so
	// its outcome/exit code propagate back verbatim. One-shot (result has no
	// value-taking flags, so dehost's first bare positional is always the id).
	if host, rest := dehost(args); host != "" {
		a.remoteVerb("result", host, rest, false)
		return
	}
	asJSON := false
	id := ""
	for _, arg := range args {
		switch arg {
		case "--json":
			asJSON = true
		default:
			if !strings.HasPrefix(arg, "-") && id == "" {
				id = arg
			}
		}
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: ax result <id> [--json]")
		os.Exit(2)
	}
	// Resolve the launch alias so the id printed at launch works even for a
	// harness that minted its own session id after launch (codex). The caller's
	// original id is echoed back in the JSON so their handle stays stable.
	asked := id
	id = resolveID(id)
	refreshReopenedTurn(id)

	m := meta.Load(id)
	// A session ax launched always has a meta sidecar (Mode set); a terminal
	// marker also proves it concluded. Nothing at all means an unknown id.
	if m.Mode == "" && m.Outcome == "" && m.Result == "" && !state.Terminal(id) {
		fmt.Fprintln(os.Stderr, "ax: no result for", id, "(unknown session, or it never concluded)")
		os.Exit(1)
	}
	outcome := resultOutcome(id, m)
	// The conclude-time snapshot can race the harness's final transcript flush
	// (claude's Stop hook fires as the turn ends, sometimes before the last
	// assistant record lands on disk). For terminal sessions, fall back to the
	// durable transcript and heal the record so later reads are stable. Pending
	// sessions are still mid-turn; after a hookless same-session reopen, their
	// transcript may still end with the prior turn's assistant output.
	if m.Result == "" && outcome != "pending" {
		cfg, _ := config.Load()
		if report := finalReport(cfg, id); report != "" {
			m.Result = report
			meta.Update(id, func(mm *meta.Meta) {
				if mm.Result == "" {
					mm.Result = report
				}
			})
		}
	}

	exit := 0
	switch {
	case m.Exit != nil:
		exit = *m.Exit
	case outcome == "failure":
		exit = 1
	}

	if asJSON {
		sessionID := ""
		if asked != id {
			sessionID = id
		}
		json.NewEncoder(os.Stdout).Encode(struct {
			ID      string `json:"id"`
			Session string `json:"session,omitempty"`
			Outcome string `json:"outcome"`
			Exit    int    `json:"exit"`
			Result  string `json:"result"`
		}{asked, sessionID, outcome, exit, m.Result})
		return
	}
	if m.Result != "" {
		fmt.Println(m.Result)
	}
	fmt.Fprintf(os.Stderr, "outcome=%s exit=%d\n", outcome, exit)
}

// resultOutcome resolves a session's outcome for `ax result`: the recorded
// Meta.Outcome when a conclude path set one, else derived from the durable
// terminal marker (done -> success, failed -> failure), else "pending".
func resultOutcome(id string, m meta.Meta) string {
	if m.Outcome != "" {
		return m.Outcome
	}
	switch {
	case state.Done(id):
		return "success"
	case state.Failed(id):
		return "failure"
	}
	return "pending"
}

// waitPoll is how often `ax wait` re-checks the durable terminal markers.
const waitPoll = 250 * time.Millisecond

func installWaitSignalCleanup(owner string) {
	signals := waitCleanupSignals()
	if owner == "" || len(signals) == 0 {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, signals...)
	go func() {
		sig := <-ch
		state.ClearWaiting(owner)
		signal.Stop(ch)
		os.Exit(waitSignalExitCode(sig))
	}()
}

type waitTargetGroup struct {
	host string // "" is local
	ids  []string
}

type waitSpec struct {
	groups  []waitTargetGroup
	all     bool
	timeout time.Duration
}

func parseWaitArgs(args []string) (waitSpec, error) {
	spec := waitSpec{all: true}
	groupIndex := map[string]int{}
	flagHost, err := waitGlobalHost(args)
	if err != nil {
		return spec, err
	}

	addTarget := func(host, id string) {
		if idx, ok := groupIndex[host]; ok {
			spec.groups[idx].ids = append(spec.groups[idx].ids, id)
			return
		}
		groupIndex[host] = len(spec.groups)
		spec.groups = append(spec.groups, waitTargetGroup{host: host, ids: []string{id}})
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--all":
			spec.all = true
		case "--any":
			spec.all = false
		case "--host":
			i++
			if i >= len(args) {
				return spec, fmt.Errorf("--host needs a host")
			}
		case "--timeout":
			i++
			if i >= len(args) {
				return spec, fmt.Errorf("--timeout needs a duration (e.g. 30s, 5m)")
			}
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return spec, fmt.Errorf("bad --timeout: %w", err)
			}
			spec.timeout = d
		default:
			if strings.HasPrefix(arg, "-") {
				continue
			}
			host, id := "", arg
			if h, bare, ok := strings.Cut(arg, "/"); ok {
				if flagHost != "" && h != flagHost {
					return spec, fmt.Errorf("host-qualified id %q conflicts with --host %s", arg, flagHost)
				}
				host, id = h, bare
			} else if flagHost != "" {
				host = flagHost
			}
			addTarget(host, id)
		}
	}
	return spec, nil
}

func waitGlobalHost(args []string) (string, error) {
	host := ""
	for i := 0; i < len(args); i++ {
		if args[i] != "--host" {
			continue
		}
		i++
		if i >= len(args) {
			return "", fmt.Errorf("--host needs a host")
		}
		if host != "" && args[i] != host {
			return "", fmt.Errorf("conflicting --host values %s and %s", host, args[i])
		}
		host = args[i]
	}
	return host, nil
}

func (s waitSpec) localOnly() ([]string, bool) {
	if len(s.groups) == 1 && s.groups[0].host == "" {
		return s.groups[0].ids, true
	}
	return nil, false
}

func (s waitSpec) singleRemoteHost() (string, bool) {
	if len(s.groups) == 1 && s.groups[0].host != "" {
		return s.groups[0].host, true
	}
	return "", false
}

func waitTargetKey(host, id string) string {
	if host == "" {
		return id
	}
	return host + "/" + id
}

func (s waitSpec) waitingIDs() []string {
	return s.waitingIDsWith(newIDResolver())
}

func (s waitSpec) waitingIDsWith(resolver idResolver) []string {
	var ids []string
	for _, g := range s.groups {
		for _, id := range g.ids {
			if g.host == "" {
				ids = append(ids, resolver.resolve(id))
			} else {
				ids = append(ids, waitTargetKey(g.host, id))
			}
		}
	}
	return ids
}

func (s waitSpec) pendingIDs(active []bool) []string {
	return s.pendingIDsWith(active, newIDResolver())
}

func (s waitSpec) pendingIDsWith(active []bool, resolver idResolver) []string {
	var pending []string
	for i, g := range s.groups {
		if i >= len(active) || !active[i] {
			continue
		}
		for _, id := range g.ids {
			if g.host != "" {
				pending = append(pending, waitTargetKey(g.host, id))
				continue
			}
			if !state.Terminal(resolver.resolve(id)) {
				pending = append(pending, id)
			}
		}
	}
	return pending
}

func waitArgsForHost(args []string, host string) []string {
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--host":
			if i+1 < len(args) {
				i++
			}
		case "--timeout":
			rest = append(rest, a)
			if i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
		default:
			if strings.HasPrefix(a, "-") {
				rest = append(rest, a)
				continue
			}
			if h, id, ok := strings.Cut(a, "/"); ok && h == host {
				rest = append(rest, id)
			} else {
				rest = append(rest, a)
			}
		}
	}
	return rest
}

// waitRoute decides whether `ax wait` can be delegated whole to one remote host.
// A purely local wait returns host=="". A wait whose targets all belong to one
// remote host returns that host and args with routing stripped, forwarded
// verbatim to the remote ax. mixed=true means the caller must use the federated
// per-host wait path.
func waitRoute(args []string) (host string, rest []string, mixed bool) {
	spec, err := parseWaitArgs(args)
	if err != nil {
		return "", nil, true
	}
	if _, ok := spec.localOnly(); ok {
		return "", waitArgsForHost(args, ""), false
	}
	if host, ok := spec.singleRemoteHost(); ok {
		return host, waitArgsForHost(args, host), false
	}
	return "", nil, true
}

// Wait blocks until one or more sessions reach a durable terminal state (done or
// failed), then exits: 0 when the awaited set succeeded, non-zero on failure, and
// 124 on --timeout (the coreutils `timeout` convention). It is the interactive
// counterpart to `--wait` on a headless launch: a caller can launch an
// interactive worker, `ax wait` its id, then `ax result` its id. Default is to
// wait for all ids; --any returns as soon as any one is terminal.
//
//	ax wait <id...> [--timeout DUR] [--all | --any]
func (a App) Wait(args []string) {
	spec, err := parseWaitArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(2)
	}
	if len(spec.groups) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ax wait <id...> [--timeout DUR] [--all|--any]")
		os.Exit(2)
	}
	if ids, ok := spec.localOnly(); ok {
		if owner := os.Getenv("AX_SESSION_ID"); owner != "" {
			installWaitSignalCleanup(owner)
		}
		os.Exit(waitFor(ids, spec.all, spec.timeout, waitPoll))
	}
	if host, ok := spec.singleRemoteHost(); ok {
		a.remoteVerb("wait", host, waitArgsForHost(args, host), true)
		return
	}
	if owner := os.Getenv("AX_SESSION_ID"); owner != "" {
		installWaitSignalCleanup(owner)
	}
	os.Exit(waitForMixed(spec, waitPoll))
}

type remoteWaitFunc func(ctx context.Context, host string, ids []string, all bool, timeout time.Duration) int

var runRemoteWait remoteWaitFunc = defaultRemoteWait
var runLocalWait = waitLocalGroup

func defaultRemoteWait(ctx context.Context, host string, ids []string, all bool, timeout time.Duration) int {
	args := append([]string{}, ids...)
	if all {
		args = append(args, "--all")
	} else {
		args = append(args, "--any")
	}
	if timeout > 0 {
		args = append(args, "--timeout", timeout.String())
	}
	return remoteVerbExitCode(ctx, "wait", host, args, true)
}

type waitGroupResult struct {
	idx  int
	code int
}

func waitForMixed(spec waitSpec, poll time.Duration) int {
	if poll <= 0 {
		poll = waitPoll
	}
	owner := os.Getenv("AX_SESSION_ID")
	if owner != "" {
		defer state.ClearWaiting(owner)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make(chan waitGroupResult, len(spec.groups))
	var wg sync.WaitGroup
	for i, g := range spec.groups {
		i, g := i, g
		wg.Add(1)
		go func() {
			defer wg.Done()
			code := 0
			if g.host == "" {
				code = runLocalWait(ctx, g.ids, spec.all, poll)
			} else {
				code = runRemoteWait(ctx, g.host, g.ids, spec.all, spec.timeout)
			}
			results <- waitGroupResult{idx: i, code: code}
		}()
	}
	finishCanceled := func(code int) int {
		cancel()
		wg.Wait()
		return code
	}
	finishAnyFailure := func() int {
		cancel()
		wg.Wait()
		for {
			select {
			case r := <-results:
				if r.code == 0 {
					return 0
				}
			default:
				return 1
			}
		}
	}

	active := make([]bool, len(spec.groups))
	for i := range active {
		active[i] = true
	}
	remaining := len(spec.groups)
	failed := false

	var timeoutC <-chan time.Time
	var timer *time.Timer
	if spec.timeout > 0 {
		timer = time.NewTimer(spec.timeout)
		timeoutC = timer.C
		defer timer.Stop()
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	resolver := newIDResolver()
	if owner != "" {
		state.MarkWaiting(owner, spec.waitingIDsWith(resolver))
	}
	for remaining > 0 {
		select {
		case r := <-results:
			if r.idx < 0 || r.idx >= len(active) || !active[r.idx] {
				continue
			}
			active[r.idx] = false
			remaining--
			switch r.code {
			case 0:
				if !spec.all {
					return finishCanceled(0)
				}
			case 1:
				if !spec.all {
					return finishAnyFailure()
				}
				failed = true
			case 124:
				return finishCanceled(124)
			default:
				return finishCanceled(r.code)
			}
			if remaining == 0 {
				if failed {
					return 1
				}
				return 0
			}
		case <-timeoutC:
			fmt.Fprintf(os.Stderr, "ax: wait timed out after %s; still running: %s\n", spec.timeout, strings.Join(spec.pendingIDsWith(active, resolver), " "))
			return finishCanceled(124)
		case <-ticker.C:
			if owner != "" {
				state.MarkWaiting(owner, spec.waitingIDsWith(resolver))
			}
		}
	}
	if failed {
		return 1
	}
	return 0
}

func waitLocalGroup(ctx context.Context, ids []string, all bool, poll time.Duration) int {
	if poll <= 0 {
		poll = waitPoll
	}
	resolver := newIDResolver()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		done := 0
		for _, id := range ids {
			rid := resolver.resolve(id)
			refreshReopenedTurn(rid)
			if state.Terminal(rid) {
				done++
			}
		}
		if (all && done == len(ids)) || (!all && done > 0) {
			return waitExitCodeWith(resolver, ids, all)
		}
		select {
		case <-ctx.Done():
			return 124
		case <-ticker.C:
		}
	}
}

// waitFor polls the durable terminal markers until the wait is satisfied (all
// ids terminal, or any id terminal with --any) or the timeout elapses, and
// returns the process exit code. Alias files are re-checked on each poll: a
// mint-its-own-id harness's alias only appears once the real session is adopted,
// which can happen while this wait is already running, so resolving once up
// front would pin the wait to a placeholder that never concludes. Split out
// from Wait so it is testable without os.Exit: 0 on success, 1 on a failure
// outcome, 124 on timeout. poll is a parameter so tests can drive it fast.
func waitFor(ids []string, all bool, timeout, poll time.Duration) int {
	owner := os.Getenv("AX_SESSION_ID")
	if owner != "" {
		defer state.ClearWaiting(owner)
	}
	resolver := newIDResolver()
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		done := 0
		resolved := make([]string, 0, len(ids))
		for _, id := range ids {
			rid := resolver.resolve(id)
			refreshReopenedTurn(rid)
			resolved = append(resolved, rid)
			if state.Terminal(rid) {
				done++
			}
		}
		if owner != "" {
			state.MarkWaiting(owner, resolved)
		}
		if (all && done == len(ids)) || (!all && done > 0) {
			return waitExitCodeWith(resolver, ids, all)
		}
		if timeout > 0 && !time.Now().Before(deadline) {
			var pending []string
			for _, id := range ids {
				rid := resolver.resolve(id)
				refreshReopenedTurn(rid)
				if !state.Terminal(rid) {
					pending = append(pending, id)
				}
			}
			fmt.Fprintf(os.Stderr, "ax: wait timed out after %s; still running: %s\n", timeout, strings.Join(pending, " "))
			return 124
		}
		time.Sleep(poll)
	}
}

// waitExitCode maps a satisfied wait to an exit code. --all succeeds only when
// every id concluded successfully (any failed id fails the wait); --any succeeds
// when at least one terminal id concluded successfully.
func waitExitCode(ids []string, all bool) int {
	return waitExitCodeWith(newIDResolver(), ids, all)
}

func waitExitCodeWith(resolver idResolver, ids []string, all bool) int {
	if all {
		for _, id := range ids {
			if !state.Done(resolver.resolve(id)) {
				return 1
			}
		}
		return 0
	}
	for _, id := range ids {
		if state.Done(resolver.resolve(id)) {
			return 0
		}
	}
	return 1
}
