package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/follow"
	"github.com/agentswitch-org/ax/internal/hosts"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/metrics"
	"github.com/agentswitch-org/ax/internal/models"
	"github.com/agentswitch-org/ax/internal/notify"
	"github.com/agentswitch-org/ax/internal/runs"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/shell"
	"github.com/agentswitch-org/ax/internal/view"
)

// nextVal consumes the value after a flag in an argv walk, exiting with a
// uniform error when it is missing, so every verb parses "--flag value" the
// same way instead of each inventing its own missing-value behavior.
func nextVal(flag string, args []string, i *int) string {
	*i++
	if *i >= len(args) {
		fmt.Fprintf(os.Stderr, "ax: %s needs a value\n", flag)
		os.Exit(2)
	}
	return args[*i]
}

// tagRoute decides host routing for `ax tag`. Only a host-qualified leading id
// (win01/uuid) routes: a self-tag (no leading positional, id from
// AX_SESSION_ID) must never leave the box, and guarding on the leading
// positional keeps a flag value that happens to contain a slash (e.g.
// --task "fix internal/app/x.go") from being mis-split into a bogus host.
func tagRoute(args []string) (host string, rest []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return dehost(args)
}

// Tag creates or updates a session's metadata sidecar (`ax tag`). The id is the
// first positional; when omitted (all flags), it is the caller's own
// AX_SESSION_ID, so a session self-reports without passing its id. Idempotent.
func (a App) Tag(args []string) {
	// A host-qualified leading id (host/id) reruns the tag on that host, so a
	// remote worker's outcome is recorded where its session lives. One-shot.
	if host, rest := tagRoute(args); host != "" {
		a.remoteVerb("tag", host, rest, false)
		return
	}
	id := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	} else {
		id = os.Getenv("AX_SESSION_ID")
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "ax: tag needs a session id (pass one, or run inside a session)")
		os.Exit(2)
	}
	id = resolveID(id)
	set := map[string]string{}
	var add, rm []string
	for i := 0; i < len(args); i++ {
		f := args[i]
		switch f {
		case "--name", "--run", "--group", "--parent", "--origin", "--outcome", "--task":
			key := f
			if key == "--group" { // deprecated alias for --run
				key = "--run"
			}
			set[key] = nextVal(f, args, &i)
		case "--add-label":
			add = append(add, nextVal(f, args, &i))
		case "--rm-label":
			rm = append(rm, nextVal(f, args, &i))
		default:
			fmt.Fprintf(os.Stderr, "ax: tag: unknown flag %q\n", f)
			os.Exit(2)
		}
	}
	// Outcome-tag choke point: a root declaring success runs the run's accept check
	// (--accept, passed via AX_ACCEPT). A non-zero check rejects the tag and prints
	// why, so the root keeps working instead of concluding on an unverified claim.
	if set["--outcome"] == "success" {
		if acc := os.Getenv("AX_ACCEPT"); acc != "" {
			if out, err := shell.Command(acc).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "ax: not done: %s\n", strings.TrimSpace(string(out)))
				os.Exit(1)
			}
		}
	}
	if err := meta.Update(id, func(m *meta.Meta) {
		if v, ok := set["--name"]; ok {
			m.Name = v
		}
		if v, ok := set["--run"]; ok {
			m.Group = v
		}
		if v, ok := set["--parent"]; ok {
			m.Parent = v
		}
		if v, ok := set["--origin"]; ok {
			m.Origin = v
		}
		if v, ok := set["--task"]; ok {
			m.Task = v
		}
		if v, ok := set["--outcome"]; ok {
			m.Outcome = v
		}
		// One label editor everywhere (the picker's `l` uses the same fold), so
		// re-tagging a key replaces its value instead of accumulating stale ones.
		for _, l := range add {
			m.Labels = session.EditLabels(m.Labels, l)
		}
		for _, l := range rm {
			m.Labels = session.EditLabels(m.Labels, "-"+l)
		}
	}); err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
	// Done-gate: fire run-success after the outcome is durably recorded. This is
	// the choke point ax authoritatively creates (AX_ACCEPT passed, meta written),
	// so a notify command that reads the run record sees the final state. Fired
	// inline, never from a background watcher.
	if set["--outcome"] == "success" {
		m := meta.Load(id)
		cfg, _ := config.Load()
		notify.Fire(cfg.Notify, notify.Event{
			ID:      id,
			State:   notify.RunSuccess,
			Summary: m.Task,
			Name:    m.Name,
			Group:   m.Group,
		})
	}
}

// Send writes input to a running session (`ax send`): short text via literal
// send-keys, multi-line via a tmux paste buffer. --interrupt sends ctrl-c first
// (redirect a worker mid-turn without killing it), --no-enter leaves the text
// unsubmitted. A host-qualified id (host/id) or --host routes over that host's
// transport, so remote workers take input with no extra caller code.
func (a App) Send(args []string) {
	// A host-qualified id (host/id) or --host routes the whole invocation to that
	// host: remoteVerb reruns `ax send <rest...>` there with stdin wired through,
	// so a remote `--stdin` reads the bytes we forward and the remote exit code
	// propagates. Route BEFORE parsing --stdin locally so the bytes are not
	// consumed on the wrong side of the transport.
	if host, rest := dehost(args); host != "" {
		a.remoteVerb("send", host, rest, false)
		return
	}
	var pos []string
	stdin, noEnter, interrupt := false, false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--stdin":
			stdin = true
		case "--no-enter":
			noEnter = true
		case "--interrupt":
			interrupt = true
		default:
			pos = append(pos, args[i])
		}
	}
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "ax: send needs a session id")
		os.Exit(2)
	}
	id := pos[0]
	text := strings.Join(pos[1:], " ")
	if stdin {
		b, _ := io.ReadAll(os.Stdin)
		text = strings.TrimRight(string(b), "\n")
	}
	id = resolveID(id) // the launch id keeps working for a mint-its-own-id harness
	if interrupt {
		a.mux.Interrupt(id)
	}
	if err := a.mux.Send(id, text, !noEnter); err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
}

// Host manages self-registered dynamic hosts (`ax host register|deregister`), so
// an ephemeral box joins the picker with no TOML editing: it drops a heartbeat
// record the picker merges, and a gone box ages out.
func (a App) Host(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ax host register --name N --transport T [--ax PATH] | ax host deregister --name N")
		os.Exit(2)
	}
	sub, args := args[0], args[1:]
	var r hosts.Record
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			r.Name = nextVal("--name", args, &i)
		case "--transport":
			r.Transport = nextVal("--transport", args, &i)
		case "--ax":
			r.Ax = nextVal("--ax", args, &i)
		}
	}
	switch sub {
	case "register":
		if r.Name == "" || r.Transport == "" {
			fmt.Fprintln(os.Stderr, "ax: host register needs --name and --transport")
			os.Exit(2)
		}
		if err := hosts.Register(r); err != nil {
			fmt.Fprintln(os.Stderr, "ax:", err)
			os.Exit(1)
		}
	case "deregister":
		hosts.Deregister(r.Name)
	default:
		fmt.Fprintf(os.Stderr, "ax: unknown host subcommand %q\n", sub)
		os.Exit(2)
	}
}

// Runs lists post-run records (`ax runs`), newest first, or streams them with
// --follow as runs conclude. --json emits the full records for an external sink.
func (a App) Runs(args []string) {
	jsonOut, foll := false, false
	for _, x := range args {
		switch x {
		case "--json":
			jsonOut = true
		case "--follow":
			foll = true
		}
	}
	if foll {
		seen := map[string]bool{}
		for _, r := range runs.List() {
			seen[r.Group] = true
		}
		for {
			for _, r := range runs.List() {
				if seen[r.Group] {
					continue
				}
				seen[r.Group] = true
				if err := printRun(r, jsonOut); err != nil {
					return // reader went away
				}
			}
			time.Sleep(time.Second)
		}
	}
	for _, r := range runs.List() {
		printRun(r, jsonOut)
	}
}

func printRun(r runs.Record, jsonOut bool) error {
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	_, err := fmt.Printf("%-12s %-10s $%-8.2f %2dw depth=%d  %s\n",
		r.Group, r.Outcome, r.Cost, r.Workers, r.MaxDepth, view.Clip(r.Task, 60))
	return err
}

// Metrics prints ax's session and run telemetry (`ax metrics`): a human table
// by default, --json for scripting, --prom for node_exporter's textfile
// collector. --prom here is ad hoc, on demand; the config-gated auto-write at
// run conclusion (see config.Metrics.Textfile) uses the same renderer.
func (a App) Metrics(args []string) {
	jsonOut, promOut := false, false
	for _, x := range args {
		switch x {
		case "--json":
			jsonOut = true
		case "--prom":
			promOut = true
		}
	}
	cfg, _ := config.Load()
	snap := metrics.Build(cfg, models.Load())
	switch {
	case jsonOut:
		json.NewEncoder(os.Stdout).Encode(snap)
	case promOut:
		fmt.Print(metrics.RenderProm(snap))
	default:
		fmt.Print(metrics.RenderTable(snap))
	}
}

// Ask is the human-in-the-loop pause (`ax ask`), called by a session: it records the
// question, blocks until a human answers (`ax reply` or a picker action), then
// prints the answer on stdout so the reply can be the next instruction. Under
// --unattended it does not block: it returns --default or fails, so no CI run
// deadlocks. The session's own id comes from AX_SESSION_ID.
func (a App) Ask(args []string) {
	def := ""
	var qparts []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--default" && i+1 < len(args) {
			def = args[i+1]
			i++
			continue
		}
		qparts = append(qparts, args[i])
	}
	question := strings.TrimSpace(strings.Join(qparts, " "))
	id := os.Getenv("AX_SESSION_ID")
	if id == "" {
		fmt.Fprintln(os.Stderr, "ax: ask must run inside a session (no AX_SESSION_ID)")
		os.Exit(2)
	}
	if os.Getenv("AX_UNATTENDED") == "1" {
		if def != "" {
			fmt.Println(def)
			return
		}
		fmt.Fprintln(os.Stderr, "ax: ask blocked, but --unattended and no --default")
		os.Exit(3)
	}
	if err := ask.Save(id, ask.Pending{Question: question, Asked: time.Now()}); err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
	// Reach a human away from the picker. A session that already tagged success is
	// presenting a result (done-review), the same test list --json uses to paint
	// the row green; otherwise it is stuck (needs-you). Fired once, before the
	// blocking poll below, and never allowed to stall the ask.
	m := meta.Load(id)
	st := notify.NeedsYou
	if m.Outcome == "success" {
		st = notify.DoneReview
	}
	cfg, _ := config.Load()
	notify.Fire(cfg.Notify, notify.Event{ID: id, State: st, Summary: question, Name: m.Name, Group: m.Group})
	for {
		p, ok := ask.Load(id)
		if !ok {
			return // question cleared without an answer (cancelled)
		}
		if p.Answered {
			fmt.Print(p.Answer)
			ask.Remove(id)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Reply answers a session's blocked `ax ask` (`ax reply <id> <answer>`), the
// human side of ask / reply. The answer is free text (it can be the next
// instruction, not just yes/no).
func (a App) Reply(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "ax: reply needs a session id and an answer")
		os.Exit(2)
	}
	id := args[0]
	answer := strings.Join(args[1:], " ")
	if err := ask.Answer(id, answer); err != nil {
		fmt.Fprintf(os.Stderr, "ax: no pending question for %q\n", id)
		os.Exit(1)
	}
}

// KillGroup cascade-kills a whole run: it signals every live member of the
// group, workers before the root, so no worker is orphaned. Remote members route
// over their host's transport, like a single kill.
func (a App) KillGroup(group string) {
	cfg, _ := config.Load()
	a.cascadeKill(cfg, group)
}

// Move relocates sessions' tmux windows into their own tmux session, for sorting
// related agents together:
//
//	ax move --tag workstream=blog            target defaults to the tag value
//	ax move --run blog [--to NAME]               a whole run
//	ax move <id>... --to NAME                    explicit sessions
//
// Only sessions with an open window move (a headless or remote session has no
// local window); the rest are reported and skipped.
func (a App) Move(args []string) {
	var tag, group, to string
	var ids []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tag":
			tag = nextVal("--tag", args, &i)
		case "--run", "--group": // --group is a deprecated alias for --run
			group = nextVal(args[i], args, &i)
		case "--to":
			to = nextVal("--to", args, &i)
		default:
			ids = append(ids, args[i])
		}
	}
	if tag == "" && group == "" && len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "ax move: need --tag k=v, --run R, or session ids")
		os.Exit(2)
	}
	if to == "" {
		switch { // a sensible session name falls out of what was selected
		case tag != "":
			if _, v, ok := strings.Cut(tag, "="); ok && v != "" {
				to = v
			} else {
				to = tag
			}
		case group != "":
			to = group
		default:
			fmt.Fprintln(os.Stderr, "ax move: --to NAME required with explicit ids")
			os.Exit(2)
		}
	}
	cfg, _ := config.Load()
	var picked []session.Session
	switch {
	case len(ids) > 0:
		for _, id := range ids {
			picked = append(picked, session.Session{ID: id})
		}
	default:
		for _, s := range session.Index(cfg) {
			if group != "" && s.Group != group {
				continue
			}
			if tag != "" && !session.HasLabel(s.Labels, tag) {
				continue
			}
			picked = append(picked, s)
		}
	}
	moved := 0
	for _, s := range picked {
		if err := a.mux.MoveWindow(s.ID, to); err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", short(s), err)
			continue
		}
		fmt.Printf("moved %s -> %s\n", short(s), to)
		moved++
	}
	if moved == 0 {
		fmt.Fprintln(os.Stderr, "ax move: nothing moved (no open windows matched)")
		os.Exit(1)
	}
}

// short is a session's friendliest handle for move/skip output.
func short(s session.Session) string {
	if s.Name != "" {
		return s.Name
	}
	if len(s.ID) >= 8 {
		return s.ID[:8]
	}
	return s.ID
}

// cascadeKill signals every session in a group, deepest first (root last). Shared
// by `ax kill --run` and a fence trip.
func (a App) cascadeKill(cfg config.Config, group string) {
	byID := map[string]session.Session{}
	var members []session.Session
	for _, s := range session.Index(cfg) {
		if s.Group == group {
			byID[s.ID] = s
			members = append(members, s)
		}
	}
	if len(members) == 0 {
		return
	}
	sortSessionsDeepestFirst(members, byID)

	// Snapshot the run record BEFORE killing: the metas below are the group
	// membership, and the root's wrapper concludes only after it dies, by which
	// time an empty snapshot would write a junk record.
	if !runs.Exists(group) {
		a.writeRun(cfg, models.Load(), group, runs.GaveUp)
	}

	hostByName := hostsByName(cfg.Hosts)
	a.killSessions(hostByName, members)
	// Keep every member's meta sidecar: it carries the durable Result/Outcome/
	// Exit a caller reads back with `ax result` after the run ends, and the
	// launch spec `ax restart` needs. killSessions already marked any member
	// that had not concluded as failed (and killCleanup preserved the terminal
	// markers), so nothing here keeps asserting a live state.
}

func sortSessionsDeepestFirst(members []session.Session, byID map[string]session.Session) {
	// Depth toward the root via parent links; kill deepest first so a parent never
	// dies before its children (which would orphan them).
	depth := func(s session.Session) int {
		d, seen := 0, map[string]bool{}
		for s.Parent != "" && !seen[s.ID] {
			seen[s.ID] = true
			p, ok := byID[s.Parent]
			if !ok {
				break
			}
			d++
			s = p
		}
		return d
	}
	sort.SliceStable(members, func(i, j int) bool { return depth(members[i]) > depth(members[j]) })
}

type readOpts struct {
	since       int
	limit       int
	follow      bool
	withContent bool
	active      bool
	fromNow     bool
	format      string // json | text
	events      map[string]bool
	timeout     time.Duration
	excludeIDs  map[string]bool
	identity    bool
}

func (o *readOpts) addExclude(raw string) {
	for _, id := range strings.Split(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if o.excludeIDs == nil {
			o.excludeIDs = map[string]bool{}
		}
		o.excludeIDs[id] = true
		if _, bare, ok := strings.Cut(id, "/"); ok && bare != "" {
			o.excludeIDs[bare] = true
			id = bare
		}
		if resolved := resolveID(id); resolved != "" {
			o.excludeIDs[resolved] = true
		}
	}
}

func (o readOpts) excludes(s session.Session) bool {
	if len(o.excludeIDs) == 0 {
		return false
	}
	return o.excludeIDs[s.ID] || o.excludeIDs[session.Key(s)]
}

// readValueFlag reports the read flags whose following argv token is data, not
// the positional id. Routing must skip these values so --exclude host/id is not
// mistaken for a remote read target.
func readValueFlag(arg string) bool {
	switch arg {
	case "--since", "--limit", "--format", "--timeout", "--events", "--exclude":
		return true
	default:
		return false
	}
}

type readRow struct {
	Host   string             `json:"host,omitempty"`
	ID     string             `json:"id"`
	Name   string             `json:"name,omitempty"`
	Group  string             `json:"group,omitempty"`
	Turns  []session.NormTurn `json:"turns"`
	Cursor int                `json:"cursor"`
}

var errReadNoMatch = errors.New("no matching session")

type errReadAmbiguous struct {
	id string
	n  int
}

func (e errReadAmbiguous) Error() string {
	return fmt.Sprintf("session id %q is ambiguous (%d live sessions match); use more characters", e.id, e.n)
}

// readRoute decides host routing for `ax read`. It strips --run first (GroupArg,
// so a run name is never mistaken for the routing id), then routes a host/id or
// --host target while preserving the forwarded argv. A --follow read streams
// unbounded (streaming=true); a plain read is one-shot.
func readRoute(args []string) (host string, rest []string, streaming bool) {
	_, args = GroupArg(args)
	rest = make([]string, 0, len(args))
	firstBare := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--follow" {
			streaming = true
		}
		if a == "--host" {
			if i+1 < len(args) {
				host = args[i+1]
				i++
			}
			continue
		}
		if readValueFlag(a) {
			rest = append(rest, a)
			if i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if firstBare && !strings.HasPrefix(a, "-") {
			firstBare = false
			if h, id, ok := strings.Cut(a, "/"); ok {
				if host == "" {
					host = h
				}
				rest = append(rest, id)
				continue
			}
		}
		rest = append(rest, a)
	}
	return host, rest, streaming
}

// Read prints a session's (or a group's) conversation as normalized turns from
// the parsed transcript, never a scraped screen. Without --follow it prints turns
// past --since and exits; with --follow it streams NDJSON turn-boundary events
// (see internal/follow). --run (or the deprecated --group) multiplexes a whole
// run into one stream.
func (a App) Read(args []string) {
	if readFederated(args) {
		a.readFederated(args)
		return
	}
	// A host-qualified id (host/id) or --host reruns the read on that host with
	// stdout wired through; a --follow read streams unbounded.
	if host, rest, streaming := readRoute(args); host != "" {
		a.remoteVerb("read", host, rest, streaming)
		return
	}
	group, args := GroupArg(args)
	o, id := parseReadArgs(args)
	id = resolveID(id) // the launch id keeps working for a mint-its-own-id harness
	cfg, _ := config.Load()
	sessions := session.Index(cfg)
	if o.follow {
		a.readFollow(cfg, sessions, id, group, o)
		return
	}
	a.readOnce(cfg, sessions, id, group, o)
}

func parseReadArgs(args []string) (readOpts, string) {
	o := readOpts{format: "json"}
	id := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--since":
			o.since, _ = strconv.Atoi(nextVal(arg, args, &i))
		case "--limit":
			o.limit, _ = strconv.Atoi(nextVal(arg, args, &i))
		case "--follow":
			o.follow = true
		case "--active":
			o.active = true
		case "--from-now":
			o.fromNow = true
		case "--with-content":
			o.withContent = true
		case "--format":
			o.format = nextVal(arg, args, &i)
		case "--timeout":
			o.timeout, _ = time.ParseDuration(nextVal(arg, args, &i))
		case "--exclude":
			o.addExclude(nextVal(arg, args, &i))
		case "--exclude-self":
			o.addExclude(os.Getenv("AX_SESSION_ID"))
		case "--events":
			o.events = map[string]bool{}
			for _, e := range strings.Split(nextVal(arg, args, &i), ",") {
				if e = strings.TrimSpace(e); e != "" {
					o.events[e] = true
				}
			}
		case "--hosts", "--federated":
			// Parsed before local routing; ignored here so remote/local self-reads
			// that receive stripped args keep the old behavior.
		case "--identity":
			// Internal: federated one-shot callers ask updated remotes to include
			// name/group so the aggregator can preserve routing identity.
			o.identity = true
		default:
			if !strings.HasPrefix(arg, "-") && id == "" {
				id = arg
			}
		}
	}
	return o, id
}

// readOnce prints the turns past the cursor once, then exits.
func (a App) readOnce(cfg config.Config, sessions []session.Session, id, group string, o readOpts) {
	rows, err := readRows(cfg, sessions, id, group, o)
	if errors.Is(err, errReadNoMatch) {
		fmt.Fprintln(os.Stderr, "ax: no matching session")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
	for _, row := range rows {
		if o.format == "text" {
			for _, t := range row.Turns {
				ts, _ := time.Parse(time.RFC3339, t.Ts)
				fmt.Printf("[%s %s] %s\n", t.Role, view.TurnTime(ts), strings.TrimSpace(t.Text))
			}
			continue
		}
		if o.identity {
			json.NewEncoder(os.Stdout).Encode(row)
			continue
		}
		json.NewEncoder(os.Stdout).Encode(struct {
			ID     string             `json:"id"`
			Turns  []session.NormTurn `json:"turns"`
			Cursor int                `json:"cursor"`
		}{row.ID, row.Turns, row.Cursor})
	}
}

func readRows(cfg config.Config, sessions []session.Session, id, group string, o readOpts) ([]readRow, error) {
	fmtOf := harnessFormats(cfg)
	targets := selectSessions(sessions, id, group)
	if group != "" {
		targets = filterExcluded(targets, o)
	}
	if o.active {
		targets = filterActive(targets, live.Snapshot())
	}
	// Fall back to a git-style unique-prefix match when the exact id missed: a
	// full v4 UUID is a mouthful, so `ax read <first-8-chars>` should reach the
	// one live local session it abbreviates. Ambiguity is an error, not a
	// silent fan-out, so a too-short prefix never reads the wrong transcript.
	if len(targets) == 0 && group == "" {
		m := prefixMatches(sessions, id)
		if o.active {
			m = filterActive(m, live.Snapshot())
		}
		if len(m) == 1 {
			targets = m
		} else if len(m) > 1 {
			return nil, errReadAmbiguous{id: id, n: len(m)}
		}
	}
	if len(targets) == 0 {
		return nil, errReadNoMatch
	}
	rows := make([]readRow, 0, len(targets))
	for _, s := range targets {
		since := o.since
		if o.fromNow {
			_, since = session.Turns(fmtOf[s.Harness], s.File, 0)
		}
		turns, cursor := session.Turns(fmtOf[s.Harness], s.File, since)
		if o.limit > 0 && len(turns) > o.limit {
			turns = turns[len(turns)-o.limit:]
		}
		rows = append(rows, readRow{
			ID:     s.ID,
			Name:   workerName(s),
			Group:  s.Group,
			Turns:  turns,
			Cursor: cursor,
		})
	}
	return rows, nil
}

// readFollow streams turn-boundary events until the session ends, --timeout, or
// the reader goes away (a broken pipe on stdout).
func (a App) readFollow(cfg config.Config, sessions []session.Session, id, group string, o readOpts) {
	ctx := context.Background()
	var cancel context.CancelFunc = func() {}
	if o.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
	}
	defer cancel()

	out := a.readFollowEvents(ctx, cfg, sessions, id, group, o)
	encodeFollowEvents(os.Stdout, out, o.limit)
}

func (a App) readFollowEvents(ctx context.Context, cfg config.Config, sessions []session.Session, id, group string, o readOpts) <-chan follow.Event {
	started := time.Now()
	base := map[string]int{}
	if o.fromNow {
		start := selectSessions(sessions, id, group)
		if group != "" {
			start = filterExcluded(start, o)
		}
		if o.active {
			start = filterActive(start, live.Snapshot())
		}
		base = startCursors(cfg, start)
	}
	fo := follow.Options{Since: o.since, WithContent: o.withContent, Events: o.events,
		PaneTail: func(sid string) string { return a.mux.PaneTail(sid, 40) }}
	out := make(chan follow.Event, 16)
	go func() {
		defer close(out) // Group/Stream return only after their senders are done
		if group != "" {
			follow.Group(ctx, func() []follow.Target {
				ss := filterGroup(session.Index(cfg), group)
				ss = filterExcluded(ss, o)
				if o.active {
					ss = filterActive(ss, live.Snapshot())
				}
				return readTargets(cfg, ss, o, base, started)
			}, fo, out)
			return
		}
		// A follow often starts right after `ax <harness>` prints the id, before
		// the session has a heartbeat or transcript; poll until it appears WITH a
		// transcript path (a meta-synthesized session has File=="" and a stream
		// started on it would never see a turn). The id is re-resolved each poll:
		// a mint-its-own-id harness's alias only appears once the real session is
		// adopted, possibly after this follow started. The ctx timeout bounds the wait.
		for {
			ss := selectSessions(sessions, resolveID(id), group)
			if o.active {
				ss = filterActive(ss, live.Snapshot())
			}
			if t := readTargets(cfg, ss, o, base, started); len(t) > 0 && t[0].File != "" {
				follow.Stream(ctx, t[0], fo, out)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				sessions = session.Index(cfg)
			}
		}
	}()
	return out
}

func encodeFollowEvents(w io.Writer, out <-chan follow.Event, limit int) {
	enc := json.NewEncoder(w)
	n := 0
	for ev := range out {
		if err := enc.Encode(ev); err != nil {
			return // reader went away (SIGPIPE)
		}
		if n++; limit > 0 && n >= limit {
			return
		}
	}
}

// selectSessions resolves the id or group to concrete local sessions.
func selectSessions(sessions []session.Session, id, group string) []session.Session {
	if group != "" {
		return filterGroup(sessions, group)
	}
	for _, s := range sessions {
		if s.ID == id && s.Host == "" {
			return []session.Session{s}
		}
	}
	return nil
}

// prefixMatches returns the local sessions whose id begins with `id`, used only
// after an exact lookup missed. It is the seam behind unambiguous short-id
// resolution: one match resolves, more than one is a reportable ambiguity.
// Federated sessions (Host != "") never prefix-resolve; a bare id addresses
// only local sessions. An empty id matches nothing.
func prefixMatches(sessions []session.Session, id string) []session.Session {
	if id == "" {
		return nil
	}
	var m []session.Session
	for _, s := range sessions {
		if s.Host == "" && strings.HasPrefix(s.ID, id) {
			m = append(m, s)
		}
	}
	return m
}

// targets converts sessions to follow targets, resolving each harness's format
// and waiting_re.
func targets(cfg config.Config, sessions []session.Session) []follow.Target {
	return readTargets(cfg, sessions, readOpts{}, nil, time.Time{})
}

func readTargets(cfg config.Config, sessions []session.Session, o readOpts, base map[string]int, started time.Time) []follow.Target {
	byName := harnessByName(cfg.Harnesses)
	var out []follow.Target
	for _, s := range sessions {
		h := byName[s.Harness]
		t := follow.Target{ID: s.ID, Name: workerName(s), Group: s.Group, Format: h.Format, File: s.File, WaitingRe: h.WaitingRe}
		if o.fromNow {
			if cursor, ok := base[s.ID]; ok {
				t.StartCursor, t.UseStartCursor = cursor, true
			} else if sessionKnownAfter(s, started) {
				t.StartCursor, t.UseStartCursor = 0, true
			} else {
				t.StartCursor, t.UseStartCursor = cursorFor(h.Format, s.File), true
			}
		}
		out = append(out, t)
	}
	return out
}

func workerName(s session.Session) string {
	if s.Name != "" {
		return s.Name
	}
	return s.Title
}

func startCursors(cfg config.Config, sessions []session.Session) map[string]int {
	byName := harnessByName(cfg.Harnesses)
	out := map[string]int{}
	for _, s := range sessions {
		out[s.ID] = cursorFor(byName[s.Harness].Format, s.File)
	}
	return out
}

func cursorFor(format, file string) int {
	_, cursor := session.Turns(format, file, 0)
	return cursor
}

func sessionKnownAfter(s session.Session, started time.Time) bool {
	if started.IsZero() {
		return false
	}
	if !s.Created.IsZero() {
		return s.Created.After(started)
	}
	if !s.Last.IsZero() {
		return s.Last.After(started)
	}
	return false
}

func filterActive(sessions []session.Session, snap map[string]live.Entry) []session.Session {
	var out []session.Session
	for _, s := range sessions {
		if e, ok := snap[s.ID]; ok && live.Running(e) {
			out = append(out, s)
		}
	}
	return out
}

func filterExcluded(sessions []session.Session, o readOpts) []session.Session {
	if len(o.excludeIDs) == 0 {
		return sessions
	}
	var out []session.Session
	for _, s := range sessions {
		if !o.excludes(s) {
			out = append(out, s)
		}
	}
	return out
}

// harnessFormats maps a harness name to its transcript format, for Turns/follow.
func harnessFormats(cfg config.Config) map[string]string {
	m := map[string]string{}
	for _, h := range cfg.Harnesses {
		m[h.Name] = h.Format
	}
	return m
}

// anyGrouped reports whether any session belongs to a run, so the picker
// only surfaces the control-layer columns when there is a run to show.
func anyGrouped(sessions []session.Session) bool {
	for _, s := range sessions {
		if s.Group != "" {
			return true
		}
	}
	return false
}

// filterGroup keeps only sessions in the given group (a parent session watching its
// own run).
func filterGroup(sessions []session.Session, group string) []session.Session {
	var out []session.Session
	for _, s := range sessions {
		if s.Group == group {
			out = append(out, s)
		}
	}
	return out
}

// GroupArg pulls a "--run R" (or the deprecated "--group G" alias) out of argv,
// returning it and the remaining args. Shared by every verb (and main's kill
// dispatch) so --run parses the same way everywhere.
func GroupArg(args []string) (group string, rest []string) {
	for i := 0; i < len(args); i++ {
		if (args[i] == "--run" || args[i] == "--group") && i+1 < len(args) {
			group = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return
}
