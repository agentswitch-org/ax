package finder

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"

	"github.com/agentswitch-org/ax/internal/adopt"
	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/fuzzy"
	"github.com/agentswitch-org/ax/internal/hold"
	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/models"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/retention"
	"github.com/agentswitch-org/ax/internal/search"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
	"github.com/agentswitch-org/ax/internal/view"
)

const (
	mNormal  = iota // browse with vim keys
	mFilter         // type to fuzzy-filter the selected column
	mContent        // type to ripgrep the transcripts
)

// scopeMode is the `t` scope filter, cycled All -> Live -> Working -> Active Run.
type scopeMode int

const (
	scopeAll       scopeMode = iota // every session, whatever its state
	scopeLive                       // live sessions (working + idle), hides inactive
	scopeWorking                    // only sessions producing output now, hides idle too
	scopeActiveRun                  // default: owners, live/waiting work, and run roll-ups
	scopeCount                      // sentinel: number of scopes, for cycling
)

// name is the scope label shown in the navbar ("" for all, which needs no tag).
func (s scopeMode) name() string {
	switch s {
	case scopeLive:
		return "live"
	case scopeWorking:
		return "working"
	case scopeActiveRun:
		return "active-run"
	}
	return ""
}

const tick = 150 * time.Millisecond

// reindexInterval throttles the live re-scan. It is only attempted on the ~1s
// meta tick, and session.Index is mtime-cached (unchanged transcripts are not
// reparsed), so a floor just below the tick lets a new session show up within
// ~1s while still coalescing bursts and keeping the scan off the render path.
const reindexInterval = 900 * time.Millisecond

// previewRevision is the local transcript version a cached preview was built
// from. Last catches parsed activity changes; mtime catches rewrites whose newest
// transcript timestamp did not move.
type previewRevision struct {
	file    string
	last    time.Time
	mtime   time.Time
	mtimeOK bool
}

// previewResult carries a finished preview fetch/render back to the UI goroutine.
type previewResult struct {
	key   string
	lines []string
	local bool
	rev   previewRevision
}

// searchResult carries a finished remote content search back to the UI goroutine.
type searchResult struct {
	query string
	hits  map[string]bool
}

type archiveEdit struct {
	archived   bool
	archivedAt time.Time
}

type archiveBatchResult struct {
	archived   int
	unarchived int
	failed     int
	firstErr   error
}

// reindexResult carries a finished background re-scan (fresh sessions + reloaded
// config) back to the UI goroutine, where it is merged preserving UI state.
type reindexResult struct {
	sessions []session.Session
	cfg      config.Config
}

// searchDebounce is how long content-mode typing must pause before ax searches
// remote hosts, so each keystroke doesn't fire an ssh round-trip.
const searchDebounce = 300 * time.Millisecond

// picker is the interactive TUI: it owns the screen, the model state, and the
// render/event loop. Ranking and search are in-process; the only work done off
// the UI goroutine is fetching a remote session's preview over its transport.
type picker struct {
	cfg config.Config
	db  models.DB
	mx  mux.Multiplexer
	sc  *screen

	all             []session.Session                         // every session, in the current sort order
	meta            map[string]view.RowMeta                   // live state, refreshed on a timer
	remoteState     map[string]state.Runtime                  // owner-reported state for remote sessions
	hosts           []view.HostStatus                         // machine roster (nil when local-only)
	hostFilter      string                                    // active machine filter ("" = all)
	groupFilter     string                                    // active run-group filter ("" = all)
	treeMode        bool                                      // render the run tree instead of a flat list
	groupBy         string                                    // pivot dimension: "" | "dir" | "run" | "host" | "tag:<key>"
	collapsed       map[string]bool                           // collapsed group keys under the current pivot
	groupRows       []groupRow                                // header rows for the current pivot (rebuilt in recompute)
	km              keys.Map                                  // the resolved (config-driven) keymap
	onKill          func([]session.Session)                   // performs a kill (injected by the app)
	onArchive       func([]ArchiveChange) map[string]error    // archives/unarchives sessions (injected by the app)
	onDetachWindows func([]session.Session)                   // closes (detaches) selected windows (injected by the app)
	onOpenWindows   func([]session.Session)                   // reopens/reattaches selected windows (injected by the app)
	remotePreview   func(session.Session) []string            // fetches a remote body (injected)
	remoteSearch    func(string) map[string]bool              // content-searches remote hosts (injected)
	reindex         func() ([]session.Session, config.Config) // fresh local scan + reloaded config (injected)
	load            func() View                               // background initial load (injected); nil means v.Sessions was already final
	hostUpdates     <-chan HostUpdate                         // streamed federation results (set by the loaded View; nil when local-only)

	// Network/health panel (key `S`) callbacks, injected by the app: the
	// configured host roster, a per-host status fetch (fanned out off the UI
	// goroutine), and the confirm-gated mutations that reuse the config
	// sync/rollback code paths. Nil when no hosts are configured.
	netHosts      func() []string                 // configured [[host]] names, in order
	netStatusHost func(string) view.NetHostStatus // fetch one host's live status row
	netSync       func(string) string             // push the profile to one host, returns a result line
	netSyncAll    func() string                   // push the profile to every host, returns a tally
	netRollback   func(string) string             // roll one host back to its latest backup
	confirmFn     func(string) bool               // test seam: overrides p.confirm for the panel's yes/no gate

	loading bool // true from open until load delivers, so the first frame can paint before it does

	awaitingBind bool // leader key pressed; the next key resolves against cfg.Binds

	notice                string                 // transient footer message (e.g. "detached 3 windows"); cleared on the next key
	suppressEmptyFallback bool                   // one recompute after an explicit hide action should stay empty
	archiveEdits          map[string]archiveEdit // confirmed archive flips that stale refresh snapshots must not undo

	mode      int
	query     string
	committed int    // a filter kept after Enter closes the input: 0, mFilter, or mContent
	filterCol int    // visible column selected when the metadata filter was opened
	filterKey string // stable key for filterCol, so column reorders keep the target
	visual    int    // visual-select anchor (index into matches), -1 when off
	visualKey string // the anchor SESSION (vim anchors to content): re-resolved when the list changes
	matches   []int  // indices into all, currently shown
	cursor    int    // index into matches
	top       int    // first visible match (scroll)
	marks     map[string]bool

	selCol   int
	sortCol  int
	sortDesc bool
	scope    scopeMode // All / Live / Working, cycled by `t`
	archive  retention.ArchiveFilter
	colPrefs []colPref // saved column-modal layout (persisted); empty = config/built-in defaults

	frame        int      // spinner frame
	preview      []string // current preview lines
	previewTop   int
	previewKey   string                     // session key the current preview body belongs to
	previewStick bool                       // pinned to the newest turn; scrolling up detaches it
	previewDirty bool                       // preview needs a (debounced) rebuild
	previewCache map[string][]string        // session preview by id (parse/fetch once)
	previewRev   map[string]previewRevision // local preview revision by id
	fetching     map[string]bool            // previews currently being fetched/rendered
	previewReady chan previewResult         // completed preview fetches/renders
	matchLines   []int                      // content-mode match line numbers
	matchIdx     int

	remoteHits  map[string]bool   // remote session keys matching remoteQuery
	remoteQuery string            // the content query remoteHits is for
	searchReady chan searchResult // completed remote content searches
	cs          *time.Timer       // remote content-search debounce
	ls          *time.Timer       // local content-search debounce (rg per keystroke is not free)
	searcher    search.Searcher   // built once; LookPath per keystroke is not free either

	metaReady    chan map[string]view.RowMeta // completed background meta refreshes
	metaInFlight bool                         // one refresh at a time
	rowText      map[string]string            // column filter text per session key/column
	selfID       string                       // the session the picker was opened from (AX_SESSION_ID)

	reindexReady    chan reindexResult // completed background re-scans
	reindexInFlight bool               // one re-scan at a time
	lastReindex     time.Time          // when the last re-scan was kicked (throttle)

	choice Choice
}

// run drives the picker to a Choice.
func (p *picker) run() (Choice, error) {
	sc, err := openScreen()
	if err != nil {
		return Choice{}, err
	}
	p.sc = sc
	defer sc.close()

	p.marks = map[string]bool{}
	p.collapsed = map[string]bool{}
	p.visual = -1
	p.previewCache = map[string][]string{}
	p.previewRev = map[string]previewRevision{}
	p.fetching = map[string]bool{}
	p.previewReady = make(chan previewResult, 4)
	p.remoteHits = map[string]bool{}
	p.searchReady = make(chan searchResult, 4)
	p.cs = time.NewTimer(time.Hour)
	p.cs.Stop()
	defer p.cs.Stop()
	p.ls = time.NewTimer(time.Hour)
	p.ls.Stop()
	defer p.ls.Stop()
	p.searcher = search.New(p.cfg)
	p.metaReady = make(chan map[string]view.RowMeta, 1)
	p.reindexReady = make(chan reindexResult, 1)
	p.rowText = map[string]string{}
	p.selfID = os.Getenv("AX_SESSION_ID")
	p.km = keys.Build(keyOverrides(p.cfg))
	prefs := loadPrefs()
	p.scope = prefs.scope()                     // remembered between popup invocations
	p.archive = prefs.archive()                 // remembered between popup invocations
	p.groupBy = prefs.groupBy()                 // remembered between popup invocations
	p.collapsed = prefs.collapsedFor(p.groupBy) // remembered, scoped to the current pivot
	p.colPrefs = prefs.Columns                  // saved column layout; applied per (re)load
	if p.load == nil {
		p.applySavedColLayout() // sync path: the Load path (re)applies in applyInitialLoad
	}
	p.sortCol = view.DefaultSortCol(p.cfg)
	p.selCol = p.sortCol
	p.sortDesc = view.DefaultDescFor(p.cfg, p.sortCol)

	// With no Load, v.Sessions was already the final answer (a caller that
	// pre-gathered everything, or a test): render it straight away. With Load,
	// the picker opens on a loading skeleton and the real data lands on
	// loadReady, so the first frame never waits on it.
	var loadReady chan View
	if p.load != nil {
		p.loading = true
		loadReady = make(chan View, 1)
		load := p.load
		go func() { loadReady <- load() }()
	} else {
		p.applySort()
		p.recompute()
		p.buildPreview()
	}

	t := time.NewTicker(tick)
	defer t.Stop()
	// preview rebuilds are debounced: moving the cursor only flags the preview
	// dirty, and we parse the transcript ~60ms after movement stops, so fast
	// scrolling never blocks on disk.
	pv := time.NewTimer(time.Hour)
	pv.Stop()
	defer pv.Stop()
	ticks := 0
	for {
		p.sc.render(p.frameLines())
		select {
		case e := <-sc.events:
			if p.handle(e) {
				return p.finish(), nil
			}
		case lv, ok := <-loadReady:
			if !ok {
				continue
			}
			if len(lv.Sessions) == 0 && lv.HostUpdates == nil {
				return Choice{}, ErrNoSessions
			}
			p.hostUpdates = lv.HostUpdates
			p.applyInitialLoad(lv)
			if len(p.all) == 0 {
				// Local came back empty but hosts are still being fetched: keep
				// the loading skeleton (not an empty table) until the first host
				// answers with rows, or ErrNoSessions when they all come up dry.
				p.loading = true
			}
		case hu, ok := <-p.hostUpdates:
			if !ok { // every host has reported
				p.hostUpdates = nil
				if p.loading {
					return Choice{}, ErrNoSessions // nothing local, nothing remote
				}
				continue
			}
			p.applyHostUpdate(hu)
		case <-pv.C:
			p.buildPreview()
			p.previewDirty = false
		case pr := <-p.previewReady:
			// a preview finished loading: cache it, and show it if that row
			// is still the one selected.
			delete(p.fetching, pr.key)
			p.previewCache[pr.key] = pr.lines
			if pr.local {
				if p.previewRev == nil {
					p.previewRev = map[string]previewRevision{}
				}
				p.previewRev[pr.key] = pr.rev
			}
			if cur, ok := p.cur(); ok && session.Key(cur) == pr.key {
				p.preview = pr.lines
				p.previewKey = pr.key
				p.placePreview()
			}
		case <-p.cs.C:
			p.kickRemoteSearch()
		case <-p.ls.C: // local content search fires after typing pauses
			p.recompute()
			p.previewDirty = true
		case m := <-p.metaReady:
			p.applyMetaReady(m)
		case rr := <-p.reindexReady:
			p.reindexInFlight = false
			p.applyReindex(rr)
		case sr := <-p.searchReady:
			// apply only if it is still the current query (drop stale in-flight ones)
			if p.mode == mContent && sr.query == strings.TrimSpace(p.query) {
				p.remoteHits = sr.hits
				p.remoteQuery = sr.query
				p.recompute()
				p.previewDirty = true
			}
		case <-sc.resize:
		case <-t.C:
			ticks++
			if ticks%7 == 0 { // ~1s: refresh live state so working->idle updates
				p.refreshMeta()
				p.kickReindex() // re-scan sessions/config on the same cadence (throttled)
			}
			if p.anyWorking() || p.loading {
				p.frame++
			}
		}
		if p.previewDirty {
			pv.Reset(60 * time.Millisecond)
		}
	}
}

// ---- model ----

func (p *picker) applyMetaReady(m map[string]view.RowMeta) {
	ref := p.cursorRef()
	p.overlayArchiveMeta(m)
	p.meta = m
	p.metaInFlight = false
	p.rowText = map[string]string{} // row cells derive from meta
	if p.treeMode {
		p.arrangeTree()
	}
	if p.treeMode || p.scope != scopeAll || p.mode == mFilter || (p.mode == mNormal && p.committed == mFilter) {
		p.recomputeKeeping(ref)
	}
	p.previewDirty = true
}

func (p *picker) applySort() {
	view.Sort(p.cfg, p.all, p.db, p.sortCol, p.sortDesc, p.meta)
	if p.treeMode {
		p.arrangeTree()
	}
}

// cycleGroup advances the run filter: all -> each group -> all, mirroring
// the machine filter, so a run can be watched one at a time.
func (p *picker) cycleGroup() {
	groups := p.groupNames()
	if len(groups) == 0 {
		return
	}
	p.groupFilter = cycleNext(groups, p.groupFilter)
	p.recompute()
	p.previewDirty = true
}

// groupNames is the set of runs present (by group id), first-seen order.
func (p *picker) groupNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range p.all {
		if s.Group != "" && !seen[s.Group] {
			seen[s.Group] = true
			out = append(out, s.Group)
		}
	}
	return out
}

// arrangeTree reorders the rows into run order (root, then its workers,
// depth-first) and stamps each row's tree glyphs, so tree mode reads like a
// process viewer. Rows with no parent in view are roots.
func (p *picker) arrangeTree() {
	idx := map[string]int{}
	for i, s := range p.all {
		idx[s.ID] = i
	}
	children := map[string][]int{}
	var roots []int
	for i, s := range p.all {
		if s.Parent != "" {
			if _, ok := idx[s.Parent]; ok {
				children[s.Parent] = append(children[s.Parent], i)
				continue
			}
		}
		roots = append(roots, i)
	}
	var ordered []session.Session
	var walk func(i int, prefix string, last bool, depth int)
	walk = func(i int, prefix string, last bool, depth int) {
		s := p.all[i]
		glyph := ""
		if depth > 0 {
			branch := "├─ "
			if last {
				branch = "└─ "
			}
			glyph = prefix + branch
		}
		m := p.meta[session.Key(s)]
		m.TreePrefix, m.Depth = glyph, depth
		p.meta[session.Key(s)] = m
		ordered = append(ordered, s)
		kids := children[s.ID]
		cp := prefix
		if depth > 0 {
			if last {
				cp += "   "
			} else {
				cp += "│  "
			}
		}
		for j, ci := range kids {
			walk(ci, cp, j == len(kids)-1, depth+1)
		}
	}
	for j, ri := range roots {
		walk(ri, "", j == len(roots)-1, 0)
	}
	p.all = ordered
}

// groupRow is one collapsible header under the group-by pivot: its identity for
// collapse state, its display label, and roll-up aggregates over its members.
type groupRow struct {
	key          string
	label        string
	count        int
	live         int
	working      int
	waiting      int
	doneResident int
	idle         int
	concluded    int
	cost         float64
	hasCost      bool
	latest       time.Time
	collapsed    bool
	members      []int // indices into p.all, so select-a-group works even collapsed
}

// groupByDims are the pivot dimensions `b` cycles through: flat, directory,
// run, host, then one per key=value tag key present (tag:workstream, ...).
func (p *picker) groupByDims() []string {
	dims := []string{"", "dir", "run", "host"}
	for _, k := range session.LabelKeys(p.all) {
		dims = append(dims, "tag:"+k)
	}
	return dims
}

// cycleGroupBy advances the pivot. Group-by and tree mode both own the row
// order, so turning one on turns the other off.
func (p *picker) cycleGroupBy() {
	dims := p.groupByDims()
	cur := 0
	for i, d := range dims {
		if d == p.groupBy {
			cur = i
			break
		}
	}
	p.groupBy = dims[(cur+1)%len(dims)]
	p.treeMode = false
	p.collapsed = map[string]bool{}
	p.saveCurrentPrefs() // sticks across popup open/close
	p.recompute()
	p.previewDirty = true
}

// saveCurrentPrefs writes the picker's persisted UI modes to disk. The saved
// column layout is carried through verbatim so a scope/group-by toggle does not
// drop it (only the column modal edits it).
func (p *picker) saveCurrentPrefs() {
	savePrefs(uiPrefs{Scope: int(p.scope), Archive: int(p.archive), GroupBy: p.groupBy, Collapsed: collapsedKeys(p.collapsed), Columns: p.colPrefs})
}

// collapsedKeys flattens a collapse set into a sorted slice for persistence.
func collapsedKeys(m map[string]bool) []string {
	var keys []string
	for k, v := range m {
		if v {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// pivotKey is a session's bucket under the current pivot: its key (collapse
// identity) and display label.
func (p *picker) pivotKey(s session.Session) (string, string) {
	switch {
	case p.groupBy == "dir":
		if s.Dir == "" {
			return "\x00nodir", "(no dir)"
		}
		return s.Dir, view.TildePath(s.Dir)
	case p.groupBy == "run":
		if s.Group == "" {
			return "\x00solo:" + session.Key(s), "(no run)"
		}
		return s.Group, s.Group
	case p.groupBy == "host":
		if s.Host == "" {
			return "\x00local", "local"
		}
		return s.Host, s.Host
	case strings.HasPrefix(p.groupBy, "tag:"):
		key := strings.TrimPrefix(p.groupBy, "tag:")
		if v := session.LabelValue(s.Labels, key); v != "" {
			return v, key + "=" + v
		}
		return "\x00untagged", "(untagged)"
	}
	return "", ""
}

// groupMatches folds the flat match list into pivot buckets: a header sentinel
// per group (encoded as -(g+1), indexing groupRows), then its members unless
// collapsed. Groups order by most recent activity; members keep the sort order.
func (p *picker) groupMatches() {
	p.groupRows = nil
	type bucket struct {
		row  groupRow
		rows []int
	}
	var order []string
	buckets := map[string]*bucket{}
	for _, i := range p.matches {
		s := p.all[i]
		key, label := p.pivotKey(s)
		b := buckets[key]
		if b == nil {
			b = &bucket{row: groupRow{key: key, label: label, collapsed: p.collapsed[key]}}
			buckets[key] = b
			order = append(order, key)
		}
		b.rows = append(b.rows, i)
		b.row.count++
		if p.meta[session.Key(s)].State == view.StateLive {
			b.row.live++
		}
		p.addRunPhaseCounts(&b.row, p.meta[session.Key(s)])
		if c := view.Cost(s, p.db); c > 0 {
			b.row.cost += c
			b.row.hasCost = true
		}
		if s.Last.After(b.row.latest) {
			b.row.latest = s.Last
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		return buckets[order[a]].row.latest.After(buckets[order[b]].row.latest)
	})
	var out []int
	for _, key := range order {
		b := buckets[key]
		if p.groupBy == "run" && p.runSoloBucket(b.rows) {
			out = append(out, b.rows[0])
			continue
		}
		if p.groupBy == "run" {
			if _, ok := p.collapsed[key]; !ok {
				p.collapsed[key] = p.runDefaultCollapsed(b.rows)
			}
			b.row.collapsed = p.collapsed[key]
		}
		b.row.members = b.rows
		p.groupRows = append(p.groupRows, b.row)
		out = append(out, -len(p.groupRows))
		if !b.row.collapsed {
			out = append(out, b.rows...)
		} else {
			for _, i := range b.rows {
				if rowMustSurfaceCollapsed(p.meta[session.Key(p.all[i])]) {
					out = append(out, i)
				}
			}
		}
	}
	p.matches = out
}

func (p *picker) runSoloBucket(rows []int) bool {
	if len(rows) != 1 {
		return false
	}
	return p.all[rows[0]].Parent == ""
}

func (p *picker) runDefaultCollapsed(rows []int) bool {
	seenChild := false
	for _, i := range rows {
		s := p.all[i]
		m := p.meta[session.Key(s)]
		if rowMustSurfaceCollapsed(m) {
			return false
		}
		if s.Parent == "" {
			continue
		}
		seenChild = true
		if m.DisplayPhase != view.PhaseLiveDoneResident && m.DisplayPhase != view.PhaseConcluded {
			return false
		}
	}
	return seenChild
}

func rowMustSurfaceCollapsed(m view.RowMeta) bool {
	return m.Waiting == "input" || m.Waiting == "auth" || m.Waiting == "children"
}

func (p *picker) addRunPhaseCounts(g *groupRow, m view.RowMeta) {
	switch m.DisplayPhase {
	case view.PhaseLiveWorking:
		g.working++
	case view.PhaseLiveDoneResident:
		g.doneResident++
	case view.PhaseConcluded:
		g.concluded++
	case view.PhaseLiveWaiting:
		if m.Waiting != "" {
			g.waiting++
		} else {
			g.idle++
		}
	}
}

// headerToggle collapses/expands the group header under the cursor. Reports
// whether the cursor was on a header (so Enter toggles instead of resuming).
func (p *picker) headerToggle() bool {
	g, ok := p.rowHeader(p.cursor)
	if !ok {
		return false
	}
	p.collapsed[g.key] = !p.collapsed[g.key]
	p.saveCurrentPrefs()
	p.recompute()
	return true
}

// setCollapsedAll folds (or unfolds) every group header under the current pivot.
// It only applies in the group-by view, where fold state exists; elsewhere it is
// a no-op. Collapsing parks the cursor on the first header, so the list does not
// leave the cursor on a row that just disappeared.
func (p *picker) setCollapsedAll(collapse bool) {
	if p.groupBy == "" || p.mode != mNormal {
		return
	}
	if collapse {
		for _, g := range p.groupRows {
			p.collapsed[g.key] = true
		}
		p.cursor = 0
	} else {
		p.collapsed = map[string]bool{}
	}
	p.saveCurrentPrefs()
	p.recompute()
	p.previewDirty = true
}

// replySel answers the highlighted session's pending question (`r`), the picker
// side of `ax reply`. It prompts for free text, so the reply can be the next
// instruction, and no-ops when nothing is waiting on that session.
func (p *picker) replySel() {
	s, ok := p.cur()
	if !ok {
		return
	}
	q, has := ask.Load(s.ID)
	if !has || q.Answered {
		return
	}
	answer := p.promptText("reply ❯ ", q.Question, "")
	if strings.TrimSpace(answer) == "" {
		return
	}
	ask.Answer(s.ID, answer)
	p.refreshMeta()
	p.recompute()
	p.previewDirty = true
}

// promptText shows a one-line text-input modal (for a reply) and returns the
// entered text, or "" on escape.
func (p *picker) promptText(label, header, initial string) string {
	buf := initial
	for {
		p.sc.render(p.overlay(p.textBox(label, header, buf)))
		e := <-p.sc.events
		switch e.t {
		case evEnter:
			return buf
		case evEsc, evCtrlC:
			return ""
		case evRune:
			buf += string(e.r)
		case evBack:
			if buf != "" {
				buf = dropLastRune(buf)
			}
		}
	}
}

// textBox renders the reply input as a centered box with the question above it.
func (p *picker) textBox(label, header, buf string) []string {
	inner := max(vwidth(label)+vwidth(buf)+1, 44)
	if w := vwidth(header); w > inner {
		inner = w
	}
	if m := p.sc.cols - 6; m > 0 && inner > m {
		inner = m
	}
	bar := strings.Repeat("─", inner+2)
	row := func(s string) string { return dim("│") + " " + padCells(s, inner) + " " + dim("│") }
	box := []string{dim("╭" + bar + "╮")}
	if header != "" {
		box = append(box, row(ansi("2", runewidth.Truncate(header, inner, "…"))), dim("├"+bar+"┤"))
	}
	box = append(box, row(ansi("1;36", label)+buf+"\x1b[7m \x1b[0m"), dim("╰"+bar+"╯"))
	return box
}

// orderLike returns members of sub in the order they appear in base.
func orderLike(base, sub []int) []int {
	in := make(map[int]bool, len(sub))
	for _, i := range sub {
		in[i] = true
	}
	out := make([]int, 0, len(sub))
	for _, i := range base {
		if in[i] {
			out = append(out, i)
		}
	}
	return out
}

type rowRef struct {
	sessionKey string
	headerKey  string
}

func (r rowRef) valid() bool { return r.sessionKey != "" || r.headerKey != "" }

func (p *picker) cursorRef() rowRef {
	if s, ok := p.cur(); ok {
		return rowRef{sessionKey: session.Key(s)}
	}
	if g, ok := p.rowHeader(p.cursor); ok {
		return rowRef{headerKey: g.key}
	}
	return rowRef{}
}

func (p *picker) recompute() { p.recomputeKeeping(p.cursorRef()) }

func (p *picker) recomputeKeeping(ref rowRef) {
	base := p.scoped()
	suppressFallback := p.suppressEmptyFallback
	p.suppressEmptyFallback = false
	if len(base) == 0 && len(p.all) > 0 && !suppressFallback {
		base = p.fallbackWhenFiltersHideAll()
	}
	// Browse mode keeps a filter committed with Enter (you filter, close the
	// input, then navigate/mark/tag the filtered set), so the effective query
	// kind is the open input, else the committed one.
	kind := p.mode
	if kind == mNormal {
		kind = p.committed
	}
	switch kind {
	case mFilter:
		p.matches = p.fuzzy(base, p.query)
		if p.mode == mNormal {
			// A committed filter is a subset, not a ranking: keep the rows in
			// the current sort order so the sort keys (s on COST, AGE) work.
			p.matches = orderLike(base, p.matches)
		}
	case mContent:
		p.matches = p.content(base, p.query)
	default:
		p.matches = base
	}
	if p.mode == mNormal && p.groupBy != "" {
		p.groupMatches() // the pivot composes over the (possibly filtered) set
	}
	if p.visualKey != "" {
		// Re-anchor the visual selection to its session: the list re-sorts and
		// refreshes underneath an active selection, and an index anchor would
		// silently select different rows.
		p.visual = p.matchIndexOf(p.visualKey)
		if p.visual < 0 {
			p.visualKey = "" // the anchor row left the view; selection ends
		}
	} else if p.visual >= len(p.matches) {
		p.visual = len(p.matches) - 1 // clamp; -1 (off) when the list emptied
	}
	if !p.restoreCursorRef(ref) {
		p.clampCursor()
	}
	p.fixScroll()
}

func (p *picker) fallbackWhenFiltersHideAll() []int {
	total := len(p.all)
	scopeKey := p.km.Key(keys.Scope)
	if scopeKey == "" {
		scopeKey = "t"
	}
	archiveKey := p.km.Key(keys.Archive)
	if archiveKey == "" {
		archiveKey = "A"
	}
	if p.scope != scopeAll {
		if unscoped := p.scopedWith(scopeAll, p.archive); len(unscoped) > 0 {
			p.notice = fmt.Sprintf("scope: %s hid all %d sessions - press %s to change scope", p.scope.name(), total, scopeKey)
			return unscoped
		}
	}
	if p.archive != retention.All {
		if unarchived := p.scopedWith(p.scope, retention.All); len(unarchived) > 0 {
			p.notice = fmt.Sprintf("archive: %s hid all %d sessions - press %s to change archive view", p.archive.Name(), total, archiveKey)
			return unarchived
		}
	}
	if p.scope != scopeAll && p.archive != retention.All {
		if unfiltered := p.scopedWith(scopeAll, retention.All); len(unfiltered) > 0 {
			p.notice = fmt.Sprintf("scope: %s and archive: %s hid all %d sessions - press %s or %s to change filters", p.scope.name(), p.archive.Name(), total, scopeKey, archiveKey)
			return unfiltered
		}
	}
	return nil
}

func (p *picker) scopedWith(scope scopeMode, archive retention.ArchiveFilter) []int {
	savedScope, savedArchive := p.scope, p.archive
	p.scope, p.archive = scope, archive
	out := p.scoped()
	p.scope, p.archive = savedScope, savedArchive
	return out
}

// inScope reports whether a row passes the current scope filter.
func (p *picker) inScope(s session.Session, m view.RowMeta) bool {
	switch p.scope {
	case scopeLive:
		return m.State == view.StateLive
	case scopeWorking:
		return m.Activity == view.Working || m.Waiting == "children"
	case scopeActiveRun:
		return activeRunRow(s, m)
	}
	return true
}

func activeRunRow(s session.Session, m view.RowMeta) bool {
	if m.Waiting == "input" || m.Waiting == "auth" || m.Waiting == "children" {
		return true
	}
	if s.Parent == "" && s.Group != "" && m.State == view.StateLive {
		return true
	}
	if m.Activity == view.Working {
		return true
	}
	switch m.DisplayPhase {
	case view.PhaseLiveWorking, view.PhaseLiveWaiting, view.PhaseLiveDoneResident, view.PhaseConcluded:
		return s.Group != "" || m.DisplayPhase == view.PhaseLiveWorking || m.Waiting != ""
	}
	return false
}

func (p *picker) inArchive(s session.Session) bool {
	switch p.archive {
	case retention.All:
		return true
	case retention.ArchivedOnly:
		return s.Archived
	default:
		return !s.Archived
	}
}

func (p *picker) scoped() []int {
	// In any run-shaped view (tree, the run pivot, or a run filter), a run is a
	// unit: if any member of a group passes the scope, keep the whole group, so a
	// finished worker still shows under its running parent instead of looking
	// killed.
	runView := p.treeMode || p.groupBy == "run" || p.groupFilter != ""
	var keepGroups map[string]bool
	if p.scope != scopeAll && runView {
		keepGroups = map[string]bool{}
		for _, s := range p.all {
			if !p.inArchive(s) {
				continue
			}
			if s.Group != "" && p.inScope(s, p.meta[session.Key(s)]) {
				keepGroups[s.Group] = true
			}
		}
	}
	out := make([]int, 0, len(p.all))
	for i, s := range p.all {
		if !p.inArchive(s) {
			continue
		}
		if p.scope != scopeAll && !p.inScope(s, p.meta[session.Key(s)]) {
			if keepGroups == nil || s.Group == "" || !keepGroups[s.Group] {
				continue
			}
		}
		if p.hostFilter != "" && view.HostLabel(s.Host) != p.hostFilter {
			continue
		}
		if p.groupFilter != "" && s.Group != p.groupFilter {
			continue
		}
		out = append(out, i)
	}
	return out
}

// fuzzy ranks base by query with the in-process matcher, scoped to the column
// selected when insert-filter mode opened.
func (p *picker) fuzzy(base []int, query string) []int {
	query = strings.TrimSpace(query)
	if query == "" {
		return base
	}
	col := p.filterColumn()
	// A structured query (key=value) means an exact filter, not a fuzzy hunt.
	// This preserves tag filtering's old no-leak behavior while still using the
	// currently selected column as the source text.
	if strings.Contains(query, "=") {
		q := strings.ToLower(query)
		res := make([]int, 0, len(base))
		for _, i := range base {
			if strings.Contains(strings.ToLower(p.columnFilterTextFor(i, col)), q) {
				res = append(res, i)
			}
		}
		return res
	}
	texts := make([]string, len(base))
	for k, i := range base {
		texts[k] = p.columnFilterTextFor(i, col)
	}
	res := make([]int, 0, len(base))
	for _, k := range fuzzy.Rank(query, texts) {
		res = append(res, base[k])
	}
	return res
}

// filterColumn is the active metadata filter target. It normally stays fixed to
// the column selected when `i` opened the input, so moving the sort cursor after
// committing a filter does not silently retarget it.
func (p *picker) filterColumn() int {
	n := view.NumCols(p.cfg)
	if n == 0 {
		return 0
	}
	if p.filterKey != "" {
		if idx := view.ColumnIndex(p.cfg, p.filterKey); idx >= 0 {
			return idx
		}
	}
	if p.filterCol >= 0 && p.filterCol < n {
		return p.filterCol
	}
	return clamp(p.selCol, 0, n-1)
}

func (p *picker) filterColumnLabel() string {
	if lab := view.ColumnLabel(p.cfg, p.filterColumn()); lab != "" {
		return lab
	}
	return "column"
}

// columnFilterTextFor is a session's selected-column value as plain text, the
// fuzzy matcher's input. Cached per session/column (typing re-ranks every row per
// keystroke) and invalidated when row metadata or mutable session fields change.
func (p *picker) columnFilterTextFor(i, col int) string {
	if p.rowText == nil {
		p.rowText = map[string]string{}
	}
	s := p.all[i]
	key := session.Key(s) + "\x00" + strconv.Itoa(col)
	if t, ok := p.rowText[key]; ok {
		return t
	}
	t := view.ColumnFilterText(p.cfg, p.db, s, p.meta[session.Key(s)], col)
	p.rowText[key] = t
	return t
}

// content keeps base sessions whose transcript matches query. Local transcripts
// are searched in-process here; remote sessions have no local file, so they are
// kept when their owning host reported a hit (remoteHits, fetched by the debounced
// kickRemoteSearch for the current query).
func (p *picker) content(base []int, query string) []int {
	query = strings.TrimSpace(query)
	if query == "" {
		return base
	}
	files := make([]string, len(base))
	var searchable []string
	for k, i := range base {
		f := view.TextFile(p.cfg, p.all[i])
		files[k] = f
		if f != "" {
			searchable = append(searchable, f)
		}
	}
	hits := p.searcher.Matches(query, searchable, 1)
	var res []int
	for k, i := range base {
		s := p.all[i]
		if s.Host != "" { // remote: use the owner-reported hits for this exact query
			if p.remoteQuery == query && p.remoteHits[session.Key(s)] {
				res = append(res, i)
			}
			continue
		}
		if files[k] == "" {
			continue
		}
		if lns, ok := hits[files[k]]; ok && len(lns) > 0 {
			res = append(res, i)
		}
	}
	return res
}

// refreshMeta rebuilds live state OFF the UI goroutine: locators mean tmux
// execs (plus a ps per unmatched pane), and doing that synchronously froze the
// render loop for tens of ms every second. The result lands on metaReady.
func (p *picker) refreshMeta() {
	if p.metaInFlight || p.mx == nil || p.loading {
		return // the initial load is already gathering this; don't race it
	}
	p.metaInFlight = true
	all := append([]session.Session(nil), p.all...) // applySort mutates p.all in place
	go func() {
		loc := adopt.Locators(p.mx, all, p.harnessSet())
		p.metaReady <- BuildMeta(all, loc, p.remoteState)
	}()
}

// kickReindex re-scans sessions and reloads config OFF the UI goroutine, so a
// new session (or a new [[harness]] in the config) shows up in an already-open
// picker without a relaunch. Throttled to reindexInterval and one at a time, so
// the transcript glob never competes with keystrokes. Result lands on
// reindexReady.
func (p *picker) kickReindex() {
	if p.reindex == nil || p.reindexInFlight {
		return
	}
	if time.Since(p.lastReindex) < reindexInterval {
		return
	}
	p.reindexInFlight = true
	p.lastReindex = time.Now()
	go func() {
		ss, cfg := p.reindex()
		p.reindexReady <- reindexResult{ss, cfg}
	}()
}

// applyReindex swaps in a fresh session scan and reloaded config without
// disturbing the browsing state: the cursor stays on its session (by id, not
// row), and the filter, sort, scope, pivot, marks, and visual selection all
// carry over. Newly created sessions appear and vanished ones drop out; row
// values (cost, ctx, activity) reflect the fresh scan.
func (p *picker) applyReindex(rr reindexResult) {
	if len(rr.sessions) == 0 {
		return // never blank the picker on a transient empty scan
	}
	ref := p.cursorRef()
	var curKey string
	var curSess session.Session
	if s, ok := p.cur(); ok {
		curKey = session.Key(s)
		curSess = s
	}
	// The column layout can change across the swap (a first grouped/tagged/
	// remote session inserts columns BEFORE the sort column), so the sort and
	// selection anchor to their column KEYS, not their numeric indices: an
	// insertion to the left used to shift the ▲/▼ arrow onto the neighboring
	// column while the rows kept sorting by the old one.
	sortKey := view.ColumnKey(p.cfg, p.sortCol)
	selKey := view.ColumnKey(p.cfg, p.selCol)
	p.cfg = rr.cfg
	p.applySavedColLayout() // re-apply the saved column modal layout over the rescanned cfg
	p.all = p.overlayArchiveSessions(rr.sessions)
	if idx := view.ColumnIndex(p.cfg, sortKey); idx >= 0 {
		p.sortCol = idx
	} else if n := view.NumCols(p.cfg); n > 0 {
		p.sortCol = view.DefaultSortCol(p.cfg)
		p.sortDesc = view.DefaultDescFor(p.cfg, p.sortCol)
	}
	if idx := view.ColumnIndex(p.cfg, selKey); idx >= 0 {
		p.selCol = idx
	} else {
		p.selCol = p.sortCol
	}
	p.rowText = map[string]string{} // row cells derive from the refreshed sessions
	if curKey != "" {
		if freshSess, ok := sessionByKey(curKey, rr.sessions); ok && curSess.Host != "" {
			// Remote: re-parsing the body means an async ssh round-trip, so dropping
			// the cache here would flash the "loading …" placeholder on every refresh.
			// Instead hold the cached lines and re-fetch in the background (guarded by
			// p.fetching); the fresh body swaps in via previewReady when it lands. Only
			// kick a fetch when the session actually advanced, to avoid ssh churn.
			if _, ok := p.previewCache[curKey]; !ok {
				delete(p.previewCache, curKey) // no cache yet: buildPreview shows loading once
			} else if p.remotePreview != nil && sessionAdvanced(curSess, freshSess) {
				p.kickRemoteFetch(freshSess)
			}
		} else if ok && curSess.Host == "" {
			// Local previews are not instant on large transcripts. Keep the cached
			// body while unchanged, and render a changed body off the UI goroutine.
			if _, cached := p.previewCache[curKey]; cached && p.localPreviewChanged(curKey, curSess, freshSess) {
				p.kickLocalFetch(freshSess)
			}
		}
	}
	p.applySort()
	p.recomputeKeeping(ref)
	p.refreshMeta() // recompute live state for the new set (new rows have no meta yet)
	p.previewDirty = true
}

// applyHostUpdate merges one streamed federation result (see View.HostUpdates)
// into the open picker: the roster and owner-reported remote state swap in
// first (refreshMeta reads them), then the merged session set goes through the
// reindex path so sort, filter, cursor, and marks carry over exactly as on a
// live re-scan. A first update that brings rows to an empty, still-loading
// picker also clears the skeleton; one that brings none (an offline host)
// only flips its roster entry.
func (p *picker) applyHostUpdate(hu HostUpdate) {
	p.remoteState = hu.RemoteState
	p.hosts = hu.Hosts
	if p.loading && len(hu.Sessions) > 0 {
		p.loading = false
	}
	p.applyReindex(reindexResult{sessions: hu.Sessions, cfg: hu.Config})
}

// applyInitialLoad merges the background-loaded View (see finder.View.Load)
// into the picker, replacing the loading skeleton with real rows. The picker
// opened with a zero Config so its first frame could paint before Load
// finished, so config-derived state (keymap, sort column) is (re)built now
// that the real config is known.
func (p *picker) applyInitialLoad(v View) {
	p.cfg = v.Config
	p.applySavedColLayout() // re-apply the saved column modal layout over the loaded cfg
	p.db = v.Models
	p.all = v.Sessions
	p.meta = v.Meta
	if p.meta == nil {
		p.meta = map[string]view.RowMeta{}
	}
	p.remoteState = v.RemoteState
	p.hosts = v.Hosts
	p.onKill = v.OnKill
	p.onArchive = v.OnArchive
	p.onDetachWindows = v.OnDetachWindows
	p.onOpenWindows = v.OnOpenWindows
	p.remotePreview = v.RemotePreview
	p.remoteSearch = v.RemoteSearch
	p.reindex = v.Reindex
	p.netHosts = v.NetHosts
	p.netStatusHost = v.NetStatusHost
	p.netSync = v.NetSync
	p.netSyncAll = v.NetSyncAll
	p.netRollback = v.NetRollback
	p.loading = false

	p.km = keys.Build(keyOverrides(p.cfg))
	p.sortCol = view.DefaultSortCol(p.cfg)
	p.selCol = p.sortCol
	p.sortDesc = view.DefaultDescFor(p.cfg, p.sortCol)

	p.applySort()
	p.recompute()
	p.buildPreview()
}

// finish stamps the picker's final loaded config/hosts onto its choice before
// returning, so a caller using View.Load (which never got a synchronous
// Config/Hosts of its own) can still act on the choice (new session, resume)
// without re-loading what Pick already gathered in the background.
func (p *picker) finish() Choice {
	p.choice.Config = p.cfg
	p.choice.Hosts = p.hosts
	p.choice.Sessions = p.all
	return p.choice
}

// harnessSet is the configured harness names, for telling a harness pane from a
// plain shell during adopt correlation.
func (p *picker) harnessSet() map[string]bool {
	m := make(map[string]bool, len(p.cfg.Harnesses))
	for _, h := range p.cfg.Harnesses {
		m[h.Name] = true
	}
	return m
}

func (p *picker) anyWorking() bool {
	for mi := range p.matches {
		if s, ok := p.rowSession(mi); ok && p.meta[session.Key(s)].Activity == view.Working {
			return true
		}
	}
	return false
}

// rowSession resolves match row mi to its session. ok is false for a group
// header row (or out of range). Every consumer of p.matches goes through this
// or rowHeader; nothing indexes p.all[p.matches[...]] raw, because a header is
// encoded as a negative sentinel and a raw index panics on it (twice, before
// these accessors existed).
func (p *picker) rowSession(mi int) (session.Session, bool) {
	if mi < 0 || mi >= len(p.matches) || p.matches[mi] < 0 {
		return session.Session{}, false
	}
	return p.all[p.matches[mi]], true
}

// rowHeader resolves match row mi to its group header, when it is one.
func (p *picker) rowHeader(mi int) (groupRow, bool) {
	if mi < 0 || mi >= len(p.matches) || p.matches[mi] >= 0 {
		return groupRow{}, false
	}
	return p.groupRows[-p.matches[mi]-1], true
}

func (p *picker) cur() (session.Session, bool) { return p.rowSession(p.cursor) }

// ---- preview ----

func (p *picker) buildPreview() {
	p.preview = nil
	p.matchLines = nil
	p.matchIdx = 0
	s, ok := p.cur()
	if !ok {
		p.previewTop = 0
		p.previewKey = ""
		return
	}
	key := session.Key(s)
	if key != p.previewKey {
		// A different session is now selected: start pinned to the newest turn and
		// drop any manual scroll carried over from the previous row. A rebuild of
		// the SAME row (a streaming refresh) keeps the user's stick/scroll state,
		// so reading history isn't yanked back to the bottom every second.
		p.previewStick = true
		p.previewTop = 0
		p.previewKey = key
	}
	q := strings.TrimSpace(p.query)
	if p.mode == mContent && q != "" {
		data, _ := os.ReadFile(view.TextFile(p.cfg, s))
		lines := strings.Split(string(data), "\n")
		lq := strings.ToLower(q)
		p.preview = make([]string, len(lines))
		for i, ln := range lines {
			ln = expandTabs(ln)
			p.preview[i] = view.Highlight(ln, q)
			if strings.Contains(strings.ToLower(ln), lq) {
				p.matchLines = append(p.matchLines, i)
			}
		}
		// Content search jumps to the first hit rather than the newest turn.
		if len(p.matchLines) > 0 {
			p.previewTop = max(p.matchLines[0]-2, 0)
		} else {
			p.previewTop = 0
		}
		return
	}
	if cached, ok := p.previewCache[key]; ok {
		p.preview = cached
		p.placePreview()
		return
	}
	if s.Host != "" && p.remotePreview != nil {
		p.fetchRemotePreview(s)
		return
	}
	if p.previewReady != nil {
		p.fetchLocalPreview(s)
		return
	}
	lines, rev := p.localPreviewLines(s)
	p.previewCache[key] = lines
	if p.previewRev == nil {
		p.previewRev = map[string]previewRevision{}
	}
	p.previewRev[key] = rev
	p.preview = lines
	p.placePreview()
}

// previewViewH is the number of transcript rows visible in the preview pane (the
// pane height less its footer line). Headless (tests) has no screen, so any
// positive height works.
func (p *picker) previewViewH() int {
	if p.sc == nil {
		return 24
	}
	return max(p.previewHeight()-1, 0)
}

// previewMaxTop is the largest previewTop that still fills the pane: scrolling
// past it would only reveal blank lines below the last turn.
func (p *picker) previewMaxTop() int {
	return max(len(p.preview)-p.previewViewH(), 0)
}

// placePreview positions the viewport after the body changes: pinned to the
// newest turn while stuck to the bottom, otherwise clamped so a shrunken body or
// a resize can't strand the view past the end.
func (p *picker) placePreview() {
	maxTop := p.previewMaxTop()
	if p.previewStick {
		p.previewTop = maxTop
		return
	}
	p.previewTop = clamp(p.previewTop, 0, maxTop)
}

// scrollPreview moves the viewport by d lines (negative scrolls up, toward older
// turns). Landing on the last screen re-attaches the auto-stick so streaming
// turns keep it pinned; any upward move detaches it so history holds still.
func (p *picker) scrollPreview(d int) {
	maxTop := p.previewMaxTop()
	p.previewTop = clamp(p.previewTop+d, 0, maxTop)
	p.previewStick = p.previewTop >= maxTop
}

// fetchRemotePreview shows a loading line and, unless a fetch for this session is
// already running, loads its body off the UI goroutine. The result arrives on
// previewReady, keeping the transport's latency off the render loop.
func (p *picker) fetchRemotePreview(s session.Session) {
	p.preview = []string{"", "  loading preview from " + s.Host + " …"}
	p.previewTop = 0
	p.kickRemoteFetch(s)
}

// kickRemoteFetch starts a background body load off the UI goroutine, guarded so
// at most one fetch per session is in flight. Unlike fetchRemotePreview it leaves
// p.preview untouched, so a refresh can re-fetch a remote body while the cached
// lines stay on screen (no "loading …" flash). The result arrives on previewReady.
func (p *picker) kickRemoteFetch(s session.Session) {
	key := session.Key(s)
	if p.fetching[key] {
		return
	}
	p.fetching[key] = true
	go func() {
		lines := p.remotePreview(s)
		for i := range lines {
			lines[i] = expandTabs(lines[i])
		}
		p.previewReady <- previewResult{key: key, lines: lines}
	}()
}

// fetchLocalPreview shows a loading line and renders the transcript off the UI
// goroutine. Reindex refreshes use kickLocalFetch directly so the cached body can
// stay visible until the fresh one lands.
func (p *picker) fetchLocalPreview(s session.Session) {
	p.preview = []string{"", "  loading preview …"}
	p.previewTop = 0
	p.kickLocalFetch(s)
}

func (p *picker) kickLocalFetch(s session.Session) {
	key := session.Key(s)
	if p.fetching[key] || p.previewReady == nil {
		return
	}
	p.fetching[key] = true
	cfg, db := p.cfg, p.db
	go func() {
		lines := strings.Split(view.Preview(cfg, db, s), "\n")
		for i := range lines {
			lines[i] = expandTabs(lines[i])
		}
		p.previewReady <- previewResult{key: key, lines: lines, local: true, rev: previewRevisionFor(s)}
	}()
}

func (p *picker) localPreviewLines(s session.Session) ([]string, previewRevision) {
	lines := strings.Split(view.Preview(p.cfg, p.db, s), "\n")
	for i := range lines {
		lines[i] = expandTabs(lines[i])
	}
	return lines, previewRevisionFor(s)
}

func (p *picker) localPreviewChanged(key string, cur, fresh session.Session) bool {
	if p.previewRev != nil {
		if rev, ok := p.previewRev[key]; ok {
			return !rev.same(fresh)
		}
	}
	return cur.File != fresh.File || !cur.Last.Equal(fresh.Last)
}

func previewRevisionFor(s session.Session) previewRevision {
	rev := previewRevision{file: s.File, last: s.Last}
	if s.File == "" {
		return rev
	}
	if st, err := os.Stat(s.File); err == nil {
		rev.mtime = st.ModTime()
		rev.mtimeOK = true
	}
	return rev
}

func (r previewRevision) same(s session.Session) bool {
	other := previewRevisionFor(s)
	if r.file != other.file || !r.last.Equal(other.last) || r.mtimeOK != other.mtimeOK {
		return false
	}
	if r.mtimeOK && !r.mtime.Equal(other.mtime) {
		return false
	}
	return true
}

func sessionByKey(key string, sessions []session.Session) (session.Session, bool) {
	for _, s := range sessions {
		if session.Key(s) == key {
			return s, true
		}
	}
	return session.Session{}, false
}

// sessionAdvanced reports whether fresh has a newer activity timestamp than cur,
// i.e. its transcript grew since the last body fetch. Used to skip needless
// remote re-fetches when nothing changed.
func sessionAdvanced(cur, fresh session.Session) bool {
	return fresh.Last.After(cur.Last)
}

// armRemoteSearch (re)starts the content-search debounce after the query changes,
// so remote hosts are searched once typing pauses rather than per keystroke.
func (p *picker) armRemoteSearch() {
	if p.mode != mContent || p.remoteSearch == nil {
		return
	}
	if !p.cs.Stop() {
		select {
		case <-p.cs.C:
		default:
		}
	}
	p.cs.Reset(searchDebounce)
}

// kickRemoteSearch runs the content search on remote hosts for the current query,
// off the UI goroutine; the result returns on searchReady. Fired by the debounce.
func (p *picker) kickRemoteSearch() {
	if p.mode != mContent || p.remoteSearch == nil {
		return
	}
	q := strings.TrimSpace(p.query)
	if q == "" {
		return
	}
	go func() { p.searchReady <- searchResult{q, p.remoteSearch(q)} }()
}

func expandTabs(s string) string { return strings.ReplaceAll(s, "\t", "    ") }

func (p *picker) stepMatch(d int) {
	if len(p.matchLines) == 0 {
		return
	}
	p.matchIdx = clamp(p.matchIdx+d, 0, len(p.matchLines)-1)
	p.previewTop = max(p.matchLines[p.matchIdx]-2, 0)
}

// ---- metrics ----

func (p *picker) metrics() view.Metrics {
	var m view.Metrics
	if p.groupBy != "" && p.mode == mNormal {
		// Pivoted: sum the group roll-ups, so a collapsed group still counts.
		// While typing (mFilter/mContent) the matches are flat and groupRows is a
		// stale pre-filter snapshot, so fall through to the plain count instead.
		inRollup := map[int]bool{}
		for _, g := range p.groupRows {
			m.Sessions += g.count
			m.Live += g.live
			m.Cost += g.cost
			for _, i := range g.members {
				inRollup[i] = true
			}
		}
		for mi := range p.matches {
			s, ok := p.rowSession(mi)
			if !ok || inRollup[p.matches[mi]] {
				continue
			}
			m.Sessions++
			if c := view.Cost(s, p.db); c > 0 {
				m.Cost += c
			}
			if p.meta[session.Key(s)].State == view.StateLive {
				m.Live++
			}
		}
		return m
	}
	for mi := range p.matches {
		s, ok := p.rowSession(mi)
		if !ok { // a group header
			continue
		}
		m.Sessions++
		if c := view.Cost(s, p.db); c > 0 {
			m.Cost += c
		}
		if p.meta[session.Key(s)].State == view.StateLive {
			m.Live++
		}
	}
	return m
}

// ---- render ----

// loadSpinner animates the loading skeleton while the picker's background
// load (finder.View.Load) is still in flight.
var loadSpinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (p *picker) listHeight() int {
	if p.sc == nil { // headless (tests): any positive height works
		return 24
	}
	h := p.sc.rows
	ph := p.previewHeight()
	lh := h - 2 /*navbar,header*/ - 1 /*prompt*/ - 1 /*sep*/ - ph
	if lh < 1 {
		lh = 1
	}
	return lh
}

func (p *picker) previewHeight() int {
	ph := (p.sc.rows - 3) * 55 / 100
	if ph < 6 {
		ph = 6
	}
	if ph > p.sc.rows-5 {
		ph = max(p.sc.rows-5, 0)
	}
	return ph
}

func (p *picker) frameLines() []string {
	w := p.sc.cols
	lh := p.listHeight()
	ph := p.previewHeight()
	var L []string

	L = append(L, fit(view.Navbar(p.modeName(), p.metrics(), p.scope.name(), p.statusSegment(), w), w))
	L = append(L, fit("  "+view.Columns(p.cfg, p.selCol, p.sortCol, p.sortDesc), w))
	for r := 0; r < lh; r++ {
		mi := p.top + r
		if mi >= len(p.matches) {
			if r == 0 && p.loading {
				L = append(L, fit("  "+ansi("2", loadSpinner[p.frame%len(loadSpinner)]+" loading sessions…"), w))
			} else if r == 0 && p.notice != "" {
				L = append(L, fit("  "+p.notice, w))
			} else {
				L = append(L, "")
			}
			continue
		}
		L = append(L, p.rowLine(mi, w))
	}
	L = append(L, fit(p.promptLine(), w))
	L = append(L, ansi("90", strings.Repeat("─", w)))
	for r := 0; r < ph-1; r++ {
		idx := p.previewTop + r
		if idx >= 0 && idx < len(p.preview) {
			L = append(L, fit(p.preview[idx], w))
		} else {
			L = append(L, "")
		}
	}
	L = append(L, fit(p.footerLine(), w))
	return L
}

// footerLine is the status line under the transcript preview: the selected
// session's activity state (with a spinner while it is actually working) and
// how long since its transcript last changed, so a long silent reasoning
// phase reads apart from a stall even though the header still says "working".
func (p *picker) footerLine() string {
	if p.notice != "" {
		return "  " + p.notice // a bulk-action result, until the next key
	}
	s, ok := p.cur()
	if !ok {
		return ""
	}
	return "  " + view.FooterLine(p.meta[session.Key(s)], s.Last, p.frame)
}

func (p *picker) rowLine(mi, w int) string {
	if g, ok := p.rowHeader(mi); ok {
		return p.headerLine(g, mi, w)
	}
	s, ok := p.rowSession(mi)
	if !ok {
		return ""
	}
	row := view.Row(p.cfg, p.db, s, p.meta[session.Key(s)], p.frame)
	mark := " "
	if p.selfID != "" && s.ID == p.selfID {
		mark = ansi("1;36", "•") // you are here: the session this popup was opened from
	}
	if p.marks[session.Key(s)] || p.inVisual(mi) {
		mark = ansi("1;35", "+")
	}
	body := mark + " " + row
	if mi == p.cursor {
		return "\x1b[7m" + padCells(view.StripANSI(body), w) + "\x1b[0m"
	}
	if p.marks[session.Key(s)] || p.inVisual(mi) {
		return tintRow(body, w) // selection reads as a block, vim-style, not a margin glyph
	}
	return fit(body, w)
}

// tintRow renders a selected row over a full-width background tint, preserving
// the cells' own colors (each inner reset re-applies the background). This is
// what makes a selection legible in every view, including grouped and tree.
func tintRow(s string, w int) string {
	const bg = "\x1b[48;5;237m"
	s = bg + strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+bg)
	out := strings.TrimSuffix(fit(s, w), "\x1b[0m")
	if pad := w - vwidth(out); pad > 0 {
		out += strings.Repeat(" ", pad) // spaces inherit the tint: a solid bar
	}
	return out + "\x1b[0m"
}

// headerLine renders one group header: a fold arrow, the group label, and its
// roll-up (count, live, cost, latest activity). Enter on it folds/unfolds.
func (p *picker) headerLine(g groupRow, mi, w int) string {
	arrow := "▾"
	if g.collapsed {
		arrow = "▸"
	}
	info := strconv.Itoa(g.count) + " session" + plural(g.count)
	var phase []string
	if g.working > 0 {
		phase = append(phase, ansi("32", strconv.Itoa(g.working)+" working"))
	}
	if g.waiting > 0 {
		phase = append(phase, ansi("36", strconv.Itoa(g.waiting)+" waiting"))
	}
	if g.doneResident > 0 {
		phase = append(phase, ansi("36", strconv.Itoa(g.doneResident)+" done-resident"))
	}
	if g.idle > 0 {
		phase = append(phase, ansi("2", strconv.Itoa(g.idle)+" idle"))
	}
	if g.concluded > 0 {
		phase = append(phase, ansi("2", strconv.Itoa(g.concluded)+" concluded"))
	}
	if len(phase) > 0 {
		info += " · " + strings.Join(phase, " / ")
	} else if g.live > 0 {
		info += " · " + ansi("32", strconv.Itoa(g.live)+" live")
	}
	if g.hasCost {
		info += " · " + view.CostShort(g.cost)
	}
	if !g.latest.IsZero() {
		info += " · " + view.Age(g.latest)
	}
	body := "  " + ansi("1;36", arrow+" "+g.label) + "  " + ansi("2", info)
	if mi == p.cursor {
		return "\x1b[7m" + padCells(view.StripANSI(body), w) + "\x1b[0m"
	}
	return fit(body, w)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (p *picker) promptLine() string {
	switch p.mode {
	case mFilter:
		return ansi("1;36", "  filter "+p.filterColumnLabel()+" ❯ ") + p.query + "\x1b[7m \x1b[0m"
	case mContent:
		return ansi("1;36", "  search ❯ ") + p.query + "\x1b[7m \x1b[0m"
	}
	// All status (mode, machine/group filter, tree, active, metrics) lives in the
	// navbar; the bottom row is only the key hint, never polluted with state.
	return "  " + ansi("2", p.hintLine())
}

// statusSegment is the header's filter/mode status: the machine filter, the active
// group filter, and tree mode, in that order. All state lives in the navbar so the
// bottom hint row stays clean. Empty when nothing is filtered and no hosts exist.
func (p *picker) statusSegment() string {
	var segs []string
	if p.loading {
		segs = append(segs, ansi("1;33", loadSpinner[p.frame%len(loadSpinner)])+" "+ansi("2", "loading"))
	}
	if len(p.hosts) > 0 {
		if p.hostFilter == "" {
			segs = append(segs, ansi("1;36", "▸")+" "+ansi("2", "all"))
		} else {
			m := ansi("1;36", "▸") + " " + p.hostFilter
			for _, h := range p.hosts {
				if h.Name == p.hostFilter {
					m = view.MachineTag(h)
				}
			}
			segs = append(segs, m)
		}
	}
	if p.groupFilter != "" {
		segs = append(segs, ansi("1;35", "run:"+p.groupFilter))
	}
	if p.treeMode {
		segs = append(segs, ansi("1;36", "tree"))
	}
	if p.groupBy != "" {
		segs = append(segs, ansi("1;36", "by:"+p.groupBy))
	}
	segs = append(segs, ansi("1;36", "archive:"+p.archive.Name()))
	switch p.committed {
	case mFilter:
		segs = append(segs, ansi("1;36", "filter:"+p.filterColumnLabel()+":"+p.query))
	case mContent:
		segs = append(segs, ansi("1;36", "search:"+p.query))
	}
	if p.visual >= 0 {
		segs = append(segs, ansi("1;33", "visual:"+strconv.Itoa(len(p.visualSessions()))))
	}
	if n := len(p.marks); n > 0 {
		segs = append(segs, ansi("1;35", "sel:"+strconv.Itoa(n)))
	}
	if p.awaitingBind {
		segs = append(segs, ansi("1;33", "bind▸"))
	}
	return strings.Join(segs, "  ")
}

// hintLine builds the normal-mode key hint from the live keymap, so it always
// shows the keys actually in effect.
func (p *picker) hintLine() string {
	// The daily set only; everything else lives under `?`. A 13-item hint row
	// is a wall, not a hint.
	parts := []string{
		p.km.Key(keys.Down) + "/" + p.km.Key(keys.Up) + " move",
		p.km.Key(keys.Filter) + " filter",
		p.km.Key(keys.Search) + " search",
		p.km.Key(keys.Visual) + " select",
		p.km.Key(keys.Label) + " tag",
		p.km.Key(keys.GroupBy) + " by",
		p.km.Key(keys.Kill) + " kill",
		p.km.Key(keys.DetachWin) + "/" + p.km.Key(keys.OpenWin) + " win",
		p.km.Key(keys.Scope) + " scope",
		p.km.Key(keys.Archive) + " view/" + p.km.Key(keys.ToggleArchived) + " archive",
		strings.ToLower(hold.DetachLabel(p.cfg.DetachPrefix, p.cfg.DetachKey)) + " detach",
		p.km.Key(keys.Help) + " more",
	}
	if len(p.cfg.Binds) > 0 { // only worth a hint once the user has configured one
		parts = append(parts, p.km.Key(keys.Bind)+" bind")
	}
	return strings.Join(parts, " · ")
}

func (p *picker) modeName() string {
	switch {
	case p.mode != mNormal:
		return "INSERT"
	case p.visual >= 0:
		return "VISUAL"
	}
	return "NORMAL"
}

// ---- input ----

func (p *picker) handle(e ev) (done bool) {
	switch p.mode {
	case mNormal:
		return p.handleNormal(e)
	default:
		return p.handleInsert(e)
	}
}

// In normal mode Enter/Esc/Ctrl-C and the arrows are structural; every other key
// is resolved through the (config-driven) keymap and dispatched.
func (p *picker) handleNormal(e ev) bool {
	if p.awaitingBind {
		return p.handleBindKey(e)
	}
	p.notice = "" // any key dismisses a lingering bulk-action status
	switch e.t {
	case evEnter:
		return p.open()
	case evCtrlC:
		return true // abort (raw mode: ctrl-c is a key, not a signal)
	case evUp:
		p.move(-1)
		return false
	case evDown:
		p.move(1)
		return false
	case evEsc: // peel back one layer: visual, then filter
		switch {
		case p.visual >= 0:
			p.clearVisual()
		case len(p.marks) > 0:
			p.marks = map[string]bool{} // Esc also deselects the accumulated set
		case p.committed != 0:
			p.committed = 0
			p.query = ""
			p.filterCol = p.selCol
			p.filterKey = view.ColumnKey(p.cfg, p.selCol)
			p.recompute()
		}
		p.previewDirty = true
		return false
	}
	return p.dispatch(p.km.Lookup(evToKey(e)))
}

// In insert mode typed runes filter the query; ctrl chords still fire their
// actions (ctrl-n dispatches Compose, ...) so you can launch without leaving
// the filter.
func (p *picker) handleInsert(e ev) bool {
	switch e.t {
	case evRune:
		p.query += string(e.r)
		p.queryChanged()
	case evBack:
		if p.query != "" {
			p.query = dropLastRune(p.query)
			p.queryChanged()
		}
	case evEsc: // cancel: drop the query and show everything again
		p.mode = mNormal
		p.query = ""
		p.committed = 0
		p.filterCol = p.selCol
		p.filterKey = view.ColumnKey(p.cfg, p.selCol)
		p.recompute()
		p.previewDirty = true
	case evEnter: // commit: close the input, keep the filtered results
		if strings.TrimSpace(p.query) != "" {
			p.committed = p.mode
		}
		p.mode = mNormal
		p.recompute()
		p.previewDirty = true
	case evUp:
		p.move(-1)
	case evDown:
		p.move(1)
	case evCtrlC:
		return true
	case evCtrl:
		return p.dispatch(p.km.Lookup(evToKey(e)))
	}
	return false
}

// queryChanged reacts to a keystroke in the input: filter mode re-ranks
// instantly (in-process, cheap with the row-text cache), while content mode
// debounces, since each run greps every transcript on disk.
func (p *picker) queryChanged() {
	if p.mode == mContent {
		p.ls.Reset(120 * time.Millisecond)
		p.armRemoteSearch()
		return
	}
	p.recompute()
	p.previewDirty = true
}

// dispatch runs a keymap action, returning true when the action ends the picker.
func (p *picker) dispatch(a keys.Action) bool {
	if p.loading && a != keys.Quit {
		// Everything else here assumes the real config/sessions are in: column
		// cycling divides by the column count, new-session needs the loaded
		// harness list, kill/open act on a selection that can't exist yet
		// (matches is empty until the load lands). Quit alone stays live, so
		// the loading skeleton is always cancelable.
		return false
	}
	switch a {
	case keys.Down:
		p.move(1)
	case keys.Up:
		p.move(-1)
	case keys.Top:
		p.cursor = 0
		p.fixScroll()
		p.previewDirty = true
	case keys.Bottom:
		p.cursor = len(p.matches) - 1
		p.fixScroll()
		p.previewDirty = true
	case keys.HalfDown:
		p.move(p.listHeight() / 2)
	case keys.HalfUp:
		p.move(-p.listHeight() / 2)
	case keys.PreviewDown:
		p.scrollPreview(1)
	case keys.PreviewUp:
		p.scrollPreview(-1)
	case keys.PreviewHalfDown:
		p.scrollPreview(p.previewViewH() / 2)
	case keys.PreviewHalfUp:
		p.scrollPreview(-p.previewViewH() / 2)
	case keys.PreviewTop:
		p.previewTop = 0
		p.previewStick = false // detach: reading the oldest history
	case keys.PreviewBottom:
		p.previewStick = true // re-attach to the newest turn
		p.previewTop = p.previewMaxTop()
	case keys.NextMatch:
		p.stepMatch(1)
	case keys.PrevMatch:
		p.stepMatch(-1)
	case keys.ColPrev:
		p.selCol = (p.selCol - 1 + view.NumCols(p.cfg)) % view.NumCols(p.cfg)
	case keys.ColNext:
		p.selCol = (p.selCol + 1) % view.NumCols(p.cfg)
	case keys.Sort:
		p.sortBy(p.selCol)
	case keys.Scope:
		p.scope = (p.scope + 1) % scopeCount // All -> Live -> Working -> Active Run -> All
		p.saveCurrentPrefs()                 // sticks across popup open/close
		p.recompute()
		p.previewDirty = true
	case keys.Archive:
		p.archive = (p.archive + 1) % 3 // active -> all -> archived -> active
		p.saveCurrentPrefs()
		p.recompute()
		p.previewDirty = true
	case keys.Machines:
		p.cycleMachine()
	case keys.Groups:
		p.cycleGroup()
	case keys.Fold:
		if p.headerToggle() {
			p.previewDirty = true
		}
	case keys.CollapseAll:
		p.setCollapsedAll(true)
	case keys.ExpandAll:
		p.setCollapsedAll(false)
	case keys.Tree:
		ref := p.cursorRef()
		p.treeMode = !p.treeMode
		p.groupBy = "" // tree and group-by both own the row order; last toggle wins
		p.applySort()
		p.recomputeKeeping(ref)
		p.previewDirty = true
	case keys.Reply:
		p.replySel()
	case keys.Filter:
		p.enter(mFilter)
	case keys.Search:
		p.enter(mContent)
	case keys.Mark:
		p.toggleMark()
	case keys.Visual:
		p.toggleVisual()
	case keys.Label:
		p.labelEditor()
	case keys.Rename:
		p.renameRow()
	case keys.ToggleArchived:
		p.toggleArchived()
	case keys.Move:
		p.moveSelected()
	case keys.DetachWin:
		p.detachWindows()
	case keys.OpenWin:
		p.reopenWindows()
	case keys.GroupBy:
		p.cycleGroupBy()
	case keys.Columns:
		p.columnEditor()
	case keys.Open:
		return p.open()
	case keys.OpenArgs:
		return p.openArgs()
	case keys.Kill:
		return p.killSel()
	case keys.Compose:
		p.choice = Choice{Compose: true}
		return true
	case keys.New:
		p.choice = Choice{New: true}
		return true
	case keys.NewArgs:
		p.choice = Choice{NewArgs: true}
		return true
	case keys.Quit:
		return true
	case keys.Net:
		p.netPanel()
	case keys.Help:
		p.showHelp()
	case keys.Bind:
		if len(p.cfg.Binds) > 0 {
			p.awaitingBind = true
		}
	}
	return false
}

func (p *picker) enter(mode int) {
	p.mode = mode
	p.query = ""
	p.committed = 0 // a fresh input replaces any committed filter
	if mode == mFilter {
		p.filterCol = p.selCol
		p.filterKey = view.ColumnKey(p.cfg, p.selCol)
	}
	p.recompute()
	p.previewDirty = true
}

func (p *picker) move(d int) {
	if len(p.matches) == 0 {
		return
	}
	p.cursor = clamp(p.cursor+d, 0, len(p.matches)-1)
	p.fixScroll()
	p.previewDirty = true
}

func (p *picker) fixScroll() {
	lh := p.listHeight()
	if p.cursor < p.top {
		p.top = p.cursor
	}
	if p.cursor >= p.top+lh {
		p.top = p.cursor - lh + 1
	}
	if p.top < 0 {
		p.top = 0
	}
}

func (p *picker) clampCursor() {
	if p.cursor >= len(p.matches) {
		p.cursor = len(p.matches) - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

func (p *picker) sortBy(col int) {
	ref := p.cursorRef()
	if p.sortCol == col {
		p.sortDesc = !p.sortDesc
	} else {
		p.sortCol = col
		p.sortDesc = view.DefaultDescFor(p.cfg, col)
	}
	p.applySort()
	p.recomputeKeeping(ref)
	p.previewDirty = true
}

func (p *picker) toggleMark() {
	if s, ok := p.cur(); ok {
		if p.marks[session.Key(s)] {
			delete(p.marks, session.Key(s))
		} else {
			p.marks[session.Key(s)] = true
		}
		p.move(1)
	}
}

// toggleVisual starts a visual selection at the cursor, or ends one by folding
// the highlighted range into the marks, vim-style: v, j/k to extend, v to keep
// the range marked (Esc drops it instead). Actions like tag and kill act on the
// live range directly, so v-jj-l works without the second v.
func (p *picker) toggleVisual() {
	if p.visual < 0 {
		if g, ok := p.rowHeader(p.cursor); ok {
			p.toggleGroupMarks(g) // v on a header selects the whole group
			return
		}
		if s, ok := p.cur(); ok {
			p.visual, p.visualKey = p.cursor, session.Key(s)
		}
		return
	}
	for _, s := range p.visualSessions() {
		p.marks[session.Key(s)] = true
	}
	p.clearVisual()
}

func (p *picker) clearVisual() { p.visual, p.visualKey = -1, "" }

// toggleGroupMarks selects (or, when fully selected, deselects) every session in
// a pivot group, collapsed members included.
func (p *picker) toggleGroupMarks(g groupRow) {
	all := true
	for _, i := range g.members {
		if !p.marks[session.Key(p.all[i])] {
			all = false
			break
		}
	}
	for _, i := range g.members {
		if all {
			delete(p.marks, session.Key(p.all[i]))
		} else {
			p.marks[session.Key(p.all[i])] = true
		}
	}
}

// matchIndexOf finds the match row showing the session with this key, -1 if gone.
func (p *picker) matchIndexOf(key string) int {
	for mi := range p.matches {
		if s, ok := p.rowSession(mi); ok && session.Key(s) == key {
			return mi
		}
	}
	return -1
}

func (p *picker) matchHeaderIndexOf(key string) int {
	for mi := range p.matches {
		if g, ok := p.rowHeader(mi); ok && g.key == key {
			return mi
		}
	}
	return -1
}

func (p *picker) restoreCursorRef(ref rowRef) bool {
	if !ref.valid() {
		return false
	}
	if ref.sessionKey != "" {
		if mi := p.matchIndexOf(ref.sessionKey); mi >= 0 {
			p.cursor = mi
			return true
		}
	}
	if ref.headerKey != "" {
		if mi := p.matchHeaderIndexOf(ref.headerKey); mi >= 0 {
			p.cursor = mi
			return true
		}
	}
	return false
}

// visualSessions is the session rows inside the active visual range (group
// headers are skipped), empty when visual select is off.
func (p *picker) visualSessions() []session.Session {
	if p.visual < 0 {
		return nil
	}
	lo, hi := p.visual, p.cursor
	if lo > hi {
		lo, hi = hi, lo
	}
	var out []session.Session
	for mi := lo; mi <= hi; mi++ {
		if s, ok := p.rowSession(mi); ok {
			out = append(out, s)
		}
	}
	return out
}

// inVisual reports whether a row is inside the active visual range.
func (p *picker) inVisual(mi int) bool {
	if p.visual < 0 {
		return false
	}
	lo, hi := p.visual, p.cursor
	if lo > hi {
		lo, hi = hi, lo
	}
	return mi >= lo && mi <= hi
}

// selection is the acted-on set: the marked rows plus the active visual range,
// or the cursor row when neither exists.
func (p *picker) selection() []session.Session {
	seen := map[string]bool{}
	var picked []session.Session
	for _, s := range p.all {
		if p.marks[session.Key(s)] {
			seen[session.Key(s)] = true
			picked = append(picked, s)
		}
	}
	for _, s := range p.visualSessions() {
		if !seen[session.Key(s)] {
			seen[session.Key(s)] = true
			picked = append(picked, s)
		}
	}
	if len(picked) == 0 {
		if s, ok := p.cur(); ok {
			picked = []session.Session{s}
		}
	}
	return picked
}

// tagSelected prompts for a label edit and applies it to the marked selection
// (or the cursor row): "key=value" sets a tag, "-key" removes it. It updates each
// local session's meta sidecar and its in-memory labels so the change shows at
// once. Remote sessions are skipped, since their meta lives on the owning host.
// labelEditor is the `l` modal: every label on the selection listed with
// coverage counts, editable in place. j/k moves, a adds, e (or Enter) edits the
// highlighted label across the selection, d deletes it, Esc closes. Edits apply
// immediately to each local selected session's sidecar.
func (p *picker) labelEditor() {
	sel := p.selection()
	if len(sel) == 0 {
		return
	}
	local := 0
	for _, s := range sel {
		if s.Host == "" {
			local++
		}
	}
	if local == 0 {
		return // remote sidecars belong to their owners
	}
	cur := 0
	for {
		items := p.labelUnion(sel)
		if cur >= len(items) {
			cur = len(items) - 1
		}
		if cur < 0 {
			cur = 0
		}
		p.sc.render(p.overlay(p.labelBox(items, cur, local)))
		e := <-p.sc.events
		switch e.t {
		case evEsc, evCtrlC:
			return
		case evUp:
			cur = max(cur-1, 0)
		case evDown:
			cur++
		case evEnter:
			if len(items) > 0 {
				p.editLabel(sel, items[cur].label)
			}
		case evRune:
			switch e.r {
			case 'q':
				return
			case 'j':
				cur++
			case 'k':
				cur = max(cur-1, 0)
			case 'a', 'n':
				if edit := strings.TrimSpace(p.promptText("add ❯ ", "key=value or a bare flag · Enter applies to the selection", "")); edit != "" {
					p.applyLabelEdit(sel, edit)
				}
			case 'e':
				if len(items) > 0 {
					p.editLabel(sel, items[cur].label)
				}
			case 'd', 'x':
				if len(items) > 0 {
					p.applyLabelEdit(sel, "-"+labelKeyOf(items[cur].label))
				}
			}
		}
	}
}

// renameRow prompts for a new display name for the focused row, prefilled with
// its current name, and applies it. Unlike the label editor this acts on the
// cursor row alone, never a multi-select.
func (p *picker) renameRow() {
	s, ok := p.cur()
	if !ok || s.Host != "" { // remote sidecars belong to their owners
		return
	}
	name := strings.TrimSpace(p.promptText("rename ❯ ", "renaming "+session.Key(s)+" · Enter applies", s.Name))
	if name == "" || name == s.Name {
		return
	}
	p.applyRename(s.ID, name)
}

// applyRename persists a session's new display name to its meta sidecar and
// updates its in-memory row so the list reflects it without a full reload.
func (p *picker) applyRename(id, name string) {
	ref := p.cursorRef()
	for i := range p.all {
		if p.all[i].Host == "" && p.all[i].ID == id {
			p.all[i].Name = name
		}
	}
	meta.Update(id, func(m *meta.Meta) { m.Name = name })
	p.rowText = map[string]string{}
	p.applySort()
	p.recomputeKeeping(ref)
	p.previewDirty = true
}

// labelItem is one distinct label across the selection and how many of the
// selected (local) sessions carry it.
type labelItem struct {
	label string
	n     int
}

// labelUnion lists every distinct label on the selection, first-seen order.
func (p *picker) labelUnion(sel []session.Session) []labelItem {
	byID := map[string]bool{}
	for _, s := range sel {
		if s.Host == "" {
			byID[s.ID] = true
		}
	}
	counts := map[string]int{}
	var order []string
	for _, s := range p.all {
		if s.Host != "" || !byID[s.ID] {
			continue
		}
		for _, l := range s.Labels {
			if counts[l] == 0 {
				order = append(order, l)
			}
			counts[l]++
		}
	}
	items := make([]labelItem, len(order))
	for i, l := range order {
		items[i] = labelItem{label: l, n: counts[l]}
	}
	return items
}

// editLabel prompts with the label prefilled and applies the replacement: the
// old label is removed from the whole selection, the new value set on it.
func (p *picker) editLabel(sel []session.Session, old string) {
	edit := strings.TrimSpace(p.promptText("edit ❯ ", "editing "+old+" · Enter applies to the selection", old))
	if edit == "" || edit == old {
		return
	}
	p.applyLabelEdit(sel, "-"+labelKeyOf(old), edit)
}

// labelKeyOf is the removal token for a label: its key for k=v, itself bare.
func labelKeyOf(l string) string {
	if k, _, ok := strings.Cut(l, "="); ok {
		return k
	}
	return l
}

// applyLabelEdit folds edits over every local selected session, updating both
// the in-memory rows and the meta sidecars, and refreshes the view.
func (p *picker) applyLabelEdit(sel []session.Session, edits ...string) {
	ref := p.cursorRef()
	target := map[string]bool{}
	for _, s := range sel {
		if s.Host == "" { // local only; can't write a remote owner's sidecar
			target[s.ID] = true
		}
	}
	for i := range p.all {
		if p.all[i].Host != "" || !target[p.all[i].ID] {
			continue
		}
		for _, edit := range edits {
			p.all[i].Labels = session.EditLabels(p.all[i].Labels, edit)
		}
		meta.Update(p.all[i].ID, func(m *meta.Meta) {
			for _, edit := range edits {
				m.Labels = session.EditLabels(m.Labels, edit)
			}
		})
	}
	p.rowText = map[string]string{} // labels feed the TAGS cell
	p.applySort()
	p.recomputeKeeping(ref)
	p.previewDirty = true
}

// toggleArchived archives visible rows and unarchives archived rows. The app owns
// persistence and remote dispatch; the picker updates only rows the callback
// reports as successful, so the current archive filter reacts immediately.
func (p *picker) toggleArchived() {
	if p.onArchive == nil {
		return
	}
	curKey := ""
	if s, ok := p.cur(); ok {
		curKey = session.Key(s)
	}
	sel := p.selection()
	if len(sel) == 0 {
		return
	}
	changes := make([]ArchiveChange, 0, len(sel))
	liveN := 0
	for _, s := range sel {
		ch := ArchiveChange{Session: s, Archived: !s.Archived}
		changes = append(changes, ch)
		if ch.Archived && p.meta[session.Key(s)].State == view.StateLive {
			liveN++
		}
	}
	if !p.askConfirm(archiveConfirmMessage(changes, liveN)) {
		return
	}
	errs := p.onArchive(changes)
	result := p.applyArchiveBatch(changes, errs, time.Now())
	p.notice = archiveNotice(result.archived, result.unarchived, result.failed, result.firstErr)
	p.marks = map[string]bool{}
	p.clearVisual()
	p.rowText = map[string]string{}
	ref := rowRef{sessionKey: curKey}
	p.applySort()
	p.suppressEmptyFallback = true
	p.recomputeKeeping(ref)
	p.settleCursorAfterArchive(curKey)
	p.previewDirty = true
}

func (p *picker) settleCursorAfterArchive(curKey string) {
	if curKey != "" {
		if mi := p.matchIndexOf(curKey); mi >= 0 {
			p.cursor = mi
			p.fixScroll()
			return
		}
	}
	if _, ok := p.cur(); ok {
		return
	}
	for mi := p.cursor; mi < len(p.matches); mi++ {
		if _, ok := p.rowSession(mi); ok {
			p.cursor = mi
			p.fixScroll()
			return
		}
	}
	for mi := p.cursor - 1; mi >= 0; mi-- {
		if _, ok := p.rowSession(mi); ok {
			p.cursor = mi
			p.fixScroll()
			return
		}
	}
}

func archiveConfirmMessage(changes []ArchiveChange, liveN int) string {
	var archiveN, unarchiveN int
	for _, ch := range changes {
		if ch.Archived {
			archiveN++
		} else {
			unarchiveN++
		}
	}
	switch {
	case archiveN > 0 && unarchiveN == 0:
		msg := "archive " + strconv.Itoa(archiveN) + " session(s)? hides from default view"
		if liveN > 0 {
			msg += " (" + strconv.Itoa(liveN) + " live)"
		}
		return msg
	case unarchiveN > 0 && archiveN == 0:
		return "unarchive " + strconv.Itoa(unarchiveN) + " session(s)? restores to default view"
	default:
		return "toggle archive state for " + strconv.Itoa(len(changes)) + " session(s)?"
	}
}

func (p *picker) applyArchiveBatch(changes []ArchiveChange, errs map[string]error, now time.Time) archiveBatchResult {
	var result archiveBatchResult
	byKey := make(map[string]int, len(p.all))
	for i := range p.all {
		byKey[session.Key(p.all[i])] = i
	}
	if p.archiveEdits == nil {
		p.archiveEdits = map[string]archiveEdit{}
	}
	for _, ch := range changes {
		key := session.Key(ch.Session)
		if err := errs[key]; err != nil {
			result.failed++
			if result.firstErr == nil {
				result.firstErr = err
			}
			continue
		}
		edit := archiveEdit{archived: ch.Archived}
		if ch.Archived {
			edit.archivedAt = now
			result.archived++
		} else {
			result.unarchived++
		}
		p.archiveEdits[key] = edit
		if i, ok := byKey[key]; ok {
			applyArchiveEditToSession(&p.all[i], edit)
		}
		if p.meta != nil {
			m := p.meta[key]
			m.Archived = ch.Archived
			p.meta[key] = m
		}
	}
	return result
}

func (p *picker) overlayArchiveSessions(sessions []session.Session) []session.Session {
	if len(p.archiveEdits) == 0 {
		return sessions
	}
	out := sessions
	copied := false
	for i := range sessions {
		edit, ok := p.archiveEdits[session.Key(sessions[i])]
		if !ok {
			continue
		}
		if !copied {
			out = append([]session.Session(nil), sessions...)
			copied = true
		}
		applyArchiveEditToSession(&out[i], edit)
	}
	return out
}

func (p *picker) overlayArchiveMeta(m map[string]view.RowMeta) {
	if len(p.archiveEdits) == 0 || len(m) == 0 {
		return
	}
	for key, edit := range p.archiveEdits {
		if row, ok := m[key]; ok {
			row.Archived = edit.archived
			m[key] = row
		}
	}
}

func applyArchiveEditToSession(s *session.Session, edit archiveEdit) {
	s.Archived = edit.archived
	if edit.archived {
		s.ArchivedAt = edit.archivedAt
		return
	}
	s.ArchivedAt = time.Time{}
}

func archiveNotice(archived, unarchived, failed int, firstErr error) string {
	var parts []string
	if archived > 0 {
		parts = append(parts, "archived "+strconv.Itoa(archived)+" session"+plural(archived))
	}
	if unarchived > 0 {
		parts = append(parts, "unarchived "+strconv.Itoa(unarchived)+" session"+plural(unarchived))
	}
	if len(parts) == 0 {
		parts = append(parts, "no archive changes")
	}
	msg := ansi("1;36", strings.Join(parts, "  ·  "))
	if failed > 0 {
		fail := "failed " + strconv.Itoa(failed)
		if failed == 1 && firstErr != nil {
			fail += ": " + firstErr.Error()
		}
		msg += ansi("1;33", "  ·  "+fail)
	}
	return msg
}

// labelBox renders the editor: labels with coverage over the selection.
func (p *picker) labelBox(items []labelItem, cur, nSel int) []string {
	inner := 44
	for _, it := range items {
		if w := vwidth(it.label) + 8; w > inner {
			inner = w
		}
	}
	if m := p.sc.cols - 6; m > 0 && inner > m {
		inner = m
	}
	bar := strings.Repeat("─", inner+2)
	row := func(s string) string { return dim("│") + " " + padCells(s, inner) + " " + dim("│") }
	title := fmt.Sprintf("labels · %d selected", nSel)
	box := []string{dim("╭" + bar + "╮"), row(ansi("1;36", title)), dim("├" + bar + "┤")}
	if len(items) == 0 {
		box = append(box, row(ansi("2", "(no labels)")))
	}
	for i, it := range items {
		cover := fmt.Sprintf("%d/%d", it.n, nSel)
		line := padCells(view.Clip(it.label, inner-len(cover)-1), inner-len(cover)-1) + " " + ansi("2", cover)
		if i == cur {
			line = "\x1b[7m" + padCells(view.StripANSI(line), inner) + "\x1b[0m"
		}
		box = append(box, row(line))
	}
	box = append(box,
		dim("├"+bar+"┤"),
		row(ansi("2", "a add · e edit · d delete · esc done")),
		dim("╰"+bar+"╯"))
	return box
}

// moveSelected relocates the selection's windows into a tmux session of the
// given name (created if missing), the picker side of `ax move`. Sessions with
// no local window (headless, remote) are skipped.
func (p *picker) moveSelected() {
	if p.mx == nil || !p.mx.Active() || !p.mx.HasWindows() {
		return
	}
	sel := p.selection()
	if len(sel) == 0 {
		return
	}
	name := strings.TrimSpace(p.promptText("move to ❯ ", "tmux session name · created if missing", ""))
	if name == "" {
		return
	}
	for _, s := range sel {
		if s.Host != "" {
			continue // a remote session's window lives on its owner
		}
		p.mx.MoveWindow(s.ID, name)
	}
	p.marks = map[string]bool{}
	p.clearVisual()
	p.refreshMeta() // window locators changed
	p.recompute()
	p.previewDirty = true
}

// windowsOpen is the subset of sel that currently has a live mux window, matched
// by the same @ax_session key (session.Key) the app opens and locates windows by.
// It is how the bulk window actions measure their own effect (the count delta
// before/after) so the footer status reflects what actually happened, not what
// was requested.
func (p *picker) windowsOpen(sel []session.Session) []session.Session {
	if p.mx == nil || !p.mx.HasWindows() {
		return nil
	}
	var out []session.Session
	for _, s := range sel {
		if _, ok := p.mx.Locate(session.Key(s)); ok {
			out = append(out, s)
		}
	}
	return out
}

// detachWindows closes the windows of the selection, detaching each dtach-held
// session so its harness process survives (a later open reattaches it). The
// app-side callback skips any session whose window is not detach-safe (closing it
// would kill the process), so this path never kills. The footer reports how many
// windows closed and how many were left up, measured by the window-presence delta
// so the numbers are the real outcome, not the request.
func (p *picker) detachWindows() {
	if p.onDetachWindows == nil {
		return
	}
	sel := p.selection()
	if len(sel) == 0 {
		return
	}
	had := len(p.windowsOpen(sel)) // selected sessions with a live window, before
	p.onDetachWindows(sel)
	skipped := len(p.windowsOpen(sel)) // still up: skipped as not detach-safe (or never had a window)
	detached := had - skipped
	p.notice = ansi("1;36", "detached "+strconv.Itoa(detached)+" window"+plural(detached))
	if skipped > 0 {
		p.notice += ansi("2", "  ·  left "+strconv.Itoa(skipped)+" up (not detach-safe)")
	}
	p.marks = map[string]bool{}
	p.clearVisual()
	p.refreshMeta() // window locators changed
	p.recompute()
	p.previewDirty = true
}

// reopenWindows reopens the windows of the selection, reattaching a detached
// (held) session to its live process or resuming one with no window. It is the
// inverse of detachWindows and reuses the app's resume/attach path (the same one
// a normal open takes), spawning each window in the background. The footer reports
// how many windows came up, by the window-presence delta.
func (p *picker) reopenWindows() {
	if p.onOpenWindows == nil {
		return
	}
	sel := p.selection()
	if len(sel) == 0 {
		return
	}
	before := len(p.windowsOpen(sel))
	p.onOpenWindows(sel)
	opened := len(p.windowsOpen(sel)) - before
	p.notice = ansi("1;36", "opened "+strconv.Itoa(opened)+" window"+plural(opened))
	p.marks = map[string]bool{}
	p.clearVisual()
	p.refreshMeta() // window locators changed
	p.recompute()
	p.previewDirty = true
}

// open resumes the selection; `E` (withArgs) applies each harness's configured
// args while Enter/`e` resume clean. Enter on a group header folds it instead,
// and Enter in visual select keeps the range marked rather than mass-opening.
func (p *picker) open() bool { return p.openWith(false) }

func (p *picker) openArgs() bool { return p.openWith(true) }

func (p *picker) openWith(withArgs bool) bool {
	if p.headerToggle() {
		return false
	}
	if p.visual >= 0 {
		p.toggleVisual()
		return false
	}
	picked := p.selection()
	if len(picked) == 0 {
		return false
	}
	p.choice = Choice{Picked: picked, WithArgs: withArgs}
	return true
}

// killSel confirms and stops the selection without leaving the picker: killing is
// a change to the list, not a place you go to, so it stays open and refreshes.
// Closing a window only detaches; this is the explicit teardown.
func (p *picker) killSel() bool {
	picked := p.selection()
	if len(picked) == 0 {
		return false
	}
	if !p.confirm("kill " + strconv.Itoa(len(picked)) + " session(s)?  stops the agent") {
		return false
	}
	if p.onKill != nil {
		p.onKill(picked)
	}
	p.marks = map[string]bool{}
	p.clearVisual()
	p.refreshMeta()
	p.recompute()
	p.previewDirty = true
	return false // stay in the picker
}

// confirm shows a centered yes/no modal (for a destructive action) and returns
// the choice, without leaving the picker.
func (p *picker) confirm(msg string) bool {
	yes := false
	for {
		p.sc.render(p.overlay(p.confirmBox(msg, yes)))
		e := <-p.sc.events
		switch e.t {
		case evEnter:
			return yes
		case evEsc, evCtrlC:
			return false
		case evLeft, evRight, evUp, evDown: // arrows toggle the highlighted button
			yes = !yes
		case evRune:
			switch e.r {
			case 'y', 'Y':
				return true
			case 'n', 'N', 'q':
				return false
			case 'h', 'l', 'j', 'k', 'H', 'L': // vim keys toggle too
				yes = !yes
			}
		}
	}
}

// confirmBox renders the yes/no modal as a centered box, matching the chooser.

// overlay composites box lines centered over the current frame, so a modal
// floats over the picker instead of replacing it with a black screen. Splicing
// is ANSI-aware on both sides of the box.
func (p *picker) overlay(box []string) []string {
	return composite(p.frameLines(), box, p.sc.rows, p.sc.cols)
}

// composite splices box lines centered over a base frame (ANSI-safe on both
// sides), shared by the picker's modals and the chooser's backdrop mode.
func composite(base, box []string, rows, cols int) []string {
	if len(box) == 0 {
		return base
	}
	boxW := 0
	for _, ln := range box {
		if w := vwidth(ln); w > boxW {
			boxW = w
		}
	}
	x := max((cols-boxW)/2, 0)
	y := max((rows-len(box))/2, 0)
	for i, ln := range box {
		r := y + i
		for r >= len(base) {
			base = append(base, "")
		}
		row := base[r]
		if vwidth(row) < x {
			row += strings.Repeat(" ", x-vwidth(row))
		}
		left := fit(row, x)
		right := ansiSkip(row, x+boxW)
		base[r] = left + padCells(ln, boxW) + "\x1b[0m" + right
	}
	return base
}

// ansiSkip drops the first n display columns of s, passing SGR escapes through
// so the remainder keeps its colors.
func ansiSkip(s string, n int) string {
	var b strings.Builder
	cur, inEsc := 0, false
	for _, r := range s {
		if inEsc {
			b.WriteRune(r)
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			b.WriteRune(r)
			continue
		}
		if cur < n {
			cur += runewidth.RuneWidth(r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (p *picker) confirmBox(msg string, yes bool) []string {
	no, ye := "  no  ", "  yes  "
	if yes {
		ye = "\x1b[7m" + ye + "\x1b[0m"
	} else {
		no = "\x1b[7m" + no + "\x1b[0m"
	}
	buttons := no + "     " + ye
	innerW := max(vwidth(msg), vwidth(buttons))
	if innerW < 24 {
		innerW = 24
	}
	bar := strings.Repeat("─", innerW+2)
	row := func(inner string) string { return dim("│") + " " + inner + " " + dim("│") }
	center := func(s string) string {
		l := max((innerW-vwidth(s))/2, 0)
		return strings.Repeat(" ", l) + s + strings.Repeat(" ", max(innerW-vwidth(s)-l, 0))
	}
	return []string{
		dim("╭" + bar + "╮"),
		row(center(ansi("1;37", msg))),
		row(strings.Repeat(" ", innerW)),
		row(center(buttons)),
		dim("╰" + bar + "╯"),
	}
}

// showHelp pauses the picker, draws the help screen (with the live keymap so it
// shows the keys actually in effect), and waits for a key.
func (p *picker) showHelp() {
	p.sc.out.WriteString("\x1b[2J\x1b[H" + strings.ReplaceAll(view.Help(p.km, hold.DetachLabel(p.cfg.DetachPrefix, p.cfg.DetachKey), p.sc.rows, p.sc.cols), "\n", "\r\n"))
	<-p.sc.events
}

// keyOverrides flattens the config's [keys] table into the map keys.Build wants.
func keyOverrides(cfg config.Config) map[string][]string {
	if len(cfg.Keys) == 0 {
		return nil
	}
	out := make(map[string][]string, len(cfg.Keys))
	for name, list := range cfg.Keys {
		out[name] = []string(list)
	}
	return out
}

// cycleMachine advances the host filter: all -> each machine -> all. Rows scope
// to the selected machine (local sessions are "local").
func (p *picker) cycleMachine() {
	names := p.machineNames()
	if len(names) == 0 {
		return
	}
	p.hostFilter = cycleNext(names, p.hostFilter)
	p.recompute()
	p.previewDirty = true
}

// cycleNext advances a filter through names, wrapping back to "" (all) after
// the last. Shared by the machine and group filters.
func cycleNext(names []string, cur string) string {
	for i, n := range names {
		if n == cur {
			if i+1 < len(names) {
				return names[i+1]
			}
			return ""
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// machineNames is the roster order for cycling: local first, then each host.
func (p *picker) machineNames() []string {
	var names []string
	for _, h := range p.hosts {
		names = append(names, h.Name)
	}
	return names
}

// ---- small helpers ----

func vwidth(s string) int { return runewidth.StringWidth(view.StripANSI(s)) }

func fit(s string, w int) string {
	if vwidth(s) <= w {
		return s
	}
	var b strings.Builder
	cur, inEsc := 0, false
	for _, r := range s {
		if inEsc {
			b.WriteRune(r)
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			b.WriteRune(r)
			continue
		}
		rw := runewidth.RuneWidth(r)
		if cur+rw > w {
			break
		}
		b.WriteRune(r)
		cur += rw
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

func padCells(s string, w int) string {
	if d := vwidth(s); d < w {
		return s + strings.Repeat(" ", w-d)
	}
	return s
}

func ansi(code, s string) string { return "\x1b[" + code + "m" + s + "\x1b[0m" }

// dim renders s in the faint (ANSI 90) color used for box chrome and secondary text.
func dim(s string) string { return ansi("90", s) }

// dropLastRune removes the final rune (not byte) so backspace works on multibyte input.
func dropLastRune(s string) string {
	if s == "" {
		return s
	}
	_, sz := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-sz]
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// BuildMeta computes per-session display state for the picker: the runtime state
// (live/crash, working/idle, dir-gone) plus the access point's own viewer-window
// locator. Local sessions are computed from this machine's heartbeats; remote
// sessions take their owner's reported state from remoteState (the access point
// cannot compute remote liveness locally). An open viewer window counts as live
// even without a heartbeat, covering a window tagged by hand or started outside
// `ax run`.
func BuildMeta(sessions []session.Session, locators map[string]string, remoteState map[string]state.Runtime) map[string]view.RowMeta {
	rt := state.ComputeAll(sessions)
	pending := ask.List() // sessions blocked on a human (`ax ask`)
	// A top-level session with a live descendant worker is supervising, not stuck:
	// an idle/blocked supervisor is waiting on its workers, not on the human, so it
	// reads calm ("waiting") instead of "needs you".
	liveDesc := liveDescendants(sessions, locators, rt, remoteState)
	meta := make(map[string]view.RowMeta, len(sessions))
	for _, s := range sessions {
		r := rt[s.ID]
		if rs, ok := remoteState[session.Key(s)]; ok { // remote: trust the owner's report
			r = rs
		}
		m := view.RowMeta{
			Locator:  locators[session.Key(s)],
			State:    r.State,
			Activity: r.Activity,
			DirGone:  s.Dir != "" && !r.DirExists,
			// The ⚠ display flag is for a HUMAN-driven session running without
			// guardrails. A task-carrying worker got its bypass flag from ax's own
			// autonomy injection, so flagging it would mark every worker and the
			// warning would mean nothing. The wire Runtime.Yolo stays truthful.
			Yolo:       r.Yolo && s.Task == "",
			Done:       r.Done,
			Failed:     r.Failed,
			FailReason: s.FailReason,
		}
		if m.Locator != "" && m.State != view.StateLive {
			// An open window outranks the heartbeat: no beat (hand-started window)
			// or a stale one (harness suspended, machine slept through a tick) is
			// still a session on screen, not a crash. Fall back to the transcript's
			// mtime for working/idle (this is what drives the spinner).
			m.State = view.StateLive
			m.Activity = state.FileActivity(s)
		}
		m.Archived = s.Archived
		// A live local session with no viewer window here is a backgrounded worker
		// (a detached/dtach run), not a frozen idle one: mark it so the picker says
		// so. Remote sessions are surfaced by the HOST column, not detached.
		if s.Host == "" && m.State == view.StateLive && m.Locator == "" {
			m.Detached = true
		}
		// A session with a parent is a worker supervised by its owner; a top-level
		// session with a live descendant worker is itself a supervisor. An idle or
		// blocked supervisor is waiting on its workers, not on the human, so it
		// reads calm ("waiting") instead of "needs you". An explicit ax ask still
		// overrides this below: a real question the owner asked reaches the human
		// even while its workers run.
		isWorker := s.Parent != ""
		supervising := !isWorker && liveDesc[s.ID]
		if s.Host == "" && m.State == view.StateLive && state.Blocked(s.ID) {
			switch {
			case isWorker:
				m.Done = true // its owner harvests it; a finished turn is done
			case supervising:
				m.Waiting = "children" // blocked between turns, but children still run: waiting on workers
			default:
				m.Waiting = "input" // the harness's own hook says it is blocked on the user
			}
		}
		if s.Host == "" && m.State == view.StateLive && state.WaitingOnChildren(s.ID) {
			m.Waiting = "children"
		}
		if r.Waiting != "" { // a remote owner reported its own blocked session
			m.Waiting = r.Waiting
		}
		// A pending ax ask is the one signal that reaches the human even while
		// workers run: applied last so a real question outranks the calm
		// waiting-on-workers state. A session that tagged success and then asked is
		// presenting its RESULT (done-review), not stuck; never synthesize a
		// terminal Done state from the question itself.
		if _, ok := pending[s.ID]; ok {
			switch {
			case s.Outcome == "success":
				m.Waiting = "done"
			default:
				m.Waiting = "input"
			}
		}
		displayRT := state.Runtime{State: m.State, Activity: m.Activity, Waiting: m.Waiting, Done: m.Done, Failed: m.Failed}
		m.Lifecycle = state.Lifecycle(displayRT)
		m.DisplayPhase = state.DisplayPhase(displayRT)
		meta[session.Key(s)] = m
	}
	return meta
}

// liveDescendants reports, per session id, whether it has at least one
// transitively-live descendant worker. It walks each live session up its parent
// chain and marks every ancestor, so a deeper worker tree still marks the root
// coordinator. Liveness mirrors the per-row derivation in BuildMeta: a heartbeat
// (or an open viewer window) locally, or the owner's reported state for a remote
// row. A supervisor with children in flight reads "waiting on workers", not
// "needs you", even when its own harness hook says it is idle/blocked.
func liveDescendants(sessions []session.Session, locators map[string]string, rt, remoteState map[string]state.Runtime) map[string]bool {
	parentOf := make(map[string]string, len(sessions))
	isLive := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		parentOf[s.ID] = s.Parent
		r := rt[s.ID]
		if rs, ok := remoteState[session.Key(s)]; ok {
			r = rs
		}
		isLive[s.ID] = r.State == state.Live || locators[session.Key(s)] != ""
	}
	out := make(map[string]bool)
	for _, s := range sessions {
		if !isLive[s.ID] {
			continue
		}
		seen := map[string]bool{}
		for p := s.Parent; p != "" && !seen[p]; p = parentOf[p] {
			seen[p] = true
			out[p] = true
		}
	}
	return out
}
