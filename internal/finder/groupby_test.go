package finder

import (
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

func pivotPicker() *picker {
	now := time.Now()
	return &picker{
		all: []session.Session{
			{ID: "g1", Dir: "/src/blog", Last: now, Labels: []string{"workstream=hd"}},
			{ID: "g2", Dir: "/src/blog", Last: now.Add(-time.Hour), Labels: []string{"workstream=hd"}},
			{ID: "x1", Dir: "/src/ax", Last: now.Add(-2 * time.Hour)},
		},
		meta:      map[string]view.RowMeta{"g1": {State: view.StateLive}},
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		km:        keys.Build(nil),
	}
}

func TestGroupByDir(t *testing.T) {
	p := pivotPicker()
	p.groupBy = "dir"
	p.recompute()
	// expect: header(-1), g1, g2, header(-2), x1
	want := []int{-1, 0, 1, -2, 2}
	if len(p.matches) != len(want) {
		t.Fatalf("matches = %v", p.matches)
	}
	for i, m := range want {
		if p.matches[i] != m {
			t.Fatalf("matches = %v, want %v", p.matches, want)
		}
	}
	g := p.groupRows[0]
	if g.count != 2 || g.live != 1 || g.label != "/src/blog" {
		t.Fatalf("aggregate wrong: %+v", g)
	}
	// cursor on a header is not a session
	p.cursor = 0
	if _, ok := p.cur(); ok {
		t.Fatal("cur() on a header should be false")
	}
	// header renders with fold arrow + roll-up
	line := view.StripANSI(p.headerLine(g, -1, 120))
	if !strings.Contains(line, "▾ /src/blog") || !strings.Contains(line, "2 sessions") || !strings.Contains(line, "1 live") {
		t.Fatalf("header line: %q", line)
	}
}

// TestGroupByFramePath walks every per-frame consumer of the match list with
// header sentinels present, so no render-loop helper indexes p.all[-1]
// (regression: anyWorking panicked on the first frame after pressing `b`).
func TestGroupByFramePath(t *testing.T) {
	p := pivotPicker()
	p.meta["g1"] = view.RowMeta{State: view.StateLive, Activity: view.Working}
	p.groupBy = "dir"
	p.recompute()
	if !p.anyWorking() {
		t.Fatal("anyWorking should see the working session past the headers")
	}
	if m := p.metrics(); m.Sessions != 3 || m.Live != 1 {
		t.Fatalf("metrics = %+v", m)
	}
	for mi := range p.matches {
		p.cursor = mi
		_ = p.rowLine(mi, 120) // must not panic on headers or rows
		p.cur()                // headers report not-a-session
	}
}

func TestGroupByCollapse(t *testing.T) {
	p := pivotPicker()
	p.groupBy = "dir"
	p.recompute()
	p.cursor = 0 // the /src/blog header
	if !p.headerToggle() {
		t.Fatal("headerToggle should report a header")
	}
	// collapsed: header(-1), header(-2), x1
	if len(p.matches) != 3 || p.matches[0] != -1 || p.matches[1] != -2 || p.matches[2] != 2 {
		t.Fatalf("collapsed matches = %v", p.matches)
	}
	if !p.headerToggle() {
		t.Fatal("expand toggle failed")
	}
	if len(p.matches) != 5 {
		t.Fatalf("expanded matches = %v", p.matches)
	}
}

func TestGroupByRunDefaultsCollapsedAndSoloRowsStayFlat(t *testing.T) {
	now := time.Now()
	p := &picker{
		all: []session.Session{
			{ID: "root", Group: "run1", Last: now},
			{ID: "worker", Group: "run1", Parent: "root", Last: now.Add(-time.Minute)},
			{ID: "solo", Last: now.Add(-2 * time.Minute)},
		},
		meta: map[string]view.RowMeta{
			"worker": {DisplayPhase: view.PhaseLiveDoneResident},
		},
		groupBy:   "run",
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		km:        keys.Build(nil),
	}
	p.recompute()
	if len(p.matches) != 2 || p.matches[0] != -1 || p.matches[1] != 2 {
		t.Fatalf("run view should show collapsed run header plus solo row, matches=%v", p.matches)
	}
	if g := p.groupRows[0]; g.key != "run1" || !g.collapsed || g.count != 2 {
		t.Fatalf("run header rollup = %+v, want collapsed run1 with 2 members", g)
	}
	p.cursor = 0
	if !p.headerToggle() {
		t.Fatal("run header should expand")
	}
	if len(p.matches) != 4 || p.matches[0] != -1 || p.matches[1] != 0 || p.matches[2] != 1 || p.matches[3] != 2 {
		t.Fatalf("expanded run matches=%v", p.matches)
	}
}

func TestGroupByRunActiveChildrenDefaultExpanded(t *testing.T) {
	now := time.Now()
	p := &picker{
		all: []session.Session{
			{ID: "root", Group: "run1", Last: now},
			{ID: "worker", Group: "run1", Parent: "root", Last: now.Add(-time.Minute)},
		},
		meta: map[string]view.RowMeta{
			"worker": {DisplayPhase: view.PhaseLiveWorking, State: view.StateLive, Activity: view.Working},
		},
		groupBy:   "run",
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		km:        keys.Build(nil),
	}
	p.recompute()
	if len(p.matches) != 3 || p.matches[0] != -1 || p.matches[1] != 0 || p.matches[2] != 1 {
		t.Fatalf("active run should default expanded, matches=%v", p.matches)
	}
	if g := p.groupRows[0]; g.collapsed || g.working != 1 {
		t.Fatalf("active run header = %+v, want expanded with 1 working", g)
	}
}

func TestGroupByRunNeedsYouRootStaysVisibleWhenChildrenDone(t *testing.T) {
	now := time.Now()
	p := &picker{
		all: []session.Session{
			{ID: "root", Group: "run1", Last: now},
			{ID: "worker", Group: "run1", Parent: "root", Last: now.Add(-time.Minute)},
		},
		meta: map[string]view.RowMeta{
			"root":   {State: view.StateLive, Activity: view.Idle, Waiting: "input", DisplayPhase: view.PhaseLiveWaiting},
			"worker": {State: view.StateLive, DisplayPhase: view.PhaseLiveDoneResident},
		},
		scope:     scopeActiveRun,
		groupBy:   "run",
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		km:        keys.Build(nil),
	}
	p.recompute()
	if len(p.matches) != 3 || p.matches[0] != -1 || p.matches[1] != 0 || p.matches[2] != 1 {
		t.Fatalf("needs-you root run should default expanded, matches=%v", p.matches)
	}
	if g := p.groupRows[0]; g.collapsed {
		t.Fatalf("needs-you root run default-collapsed: %+v", g)
	}

	p.collapsed = map[string]bool{"run1": true}
	p.recompute()
	if len(p.matches) != 2 || p.matches[0] != -1 || p.matches[1] != 0 {
		t.Fatalf("collapsed needs-you run should still surface root, matches=%v", p.matches)
	}
}

// TestGroupByCollapseExpandAll covers `-`/`=`: collapse-all folds every header,
// expand-all unfolds them, and the single-group toggle still works after.
func TestGroupByCollapseExpandAll(t *testing.T) {
	p := pivotPicker()
	p.groupBy = "dir"
	p.recompute()

	// `-`: both headers fold, only the two header sentinels remain.
	p.setCollapsedAll(true)
	if len(p.matches) != 2 || p.matches[0] != -1 || p.matches[1] != -2 {
		t.Fatalf("collapse-all matches = %v", p.matches)
	}
	if p.cursor != 0 {
		t.Fatalf("collapse-all cursor = %d, want 0", p.cursor)
	}

	// `=`: everything unfolds again.
	p.setCollapsedAll(false)
	if len(p.matches) != 5 {
		t.Fatalf("expand-all matches = %v", p.matches)
	}
	if len(p.collapsed) != 0 {
		t.Fatalf("expand-all left collapsed state: %v", p.collapsed)
	}

	// The single-group toggle still folds just one group after collapse/expand-all.
	p.cursor = 0
	if !p.headerToggle() {
		t.Fatal("single toggle after expand-all should report a header")
	}
	if len(p.matches) != 3 { // header(-1), header(-2), x1 ... blog folded, ax open
		t.Fatalf("single fold after expand-all matches = %v", p.matches)
	}
}

func TestGroupByTagAndCycle(t *testing.T) {
	p := pivotPicker()
	dims := p.groupByDims()
	// flat, dir, run, host, then the tag key present on the sessions
	want := []string{"", "dir", "run", "host", "tag:workstream"}
	if len(dims) != len(want) {
		t.Fatalf("dims = %v", dims)
	}
	for i := range want {
		if dims[i] != want[i] {
			t.Fatalf("dims = %v, want %v", dims, want)
		}
	}
	p.groupBy = "tag:workstream"
	p.recompute()
	// two buckets: workstream=hd (g1,g2) and (untagged) (x1)
	if len(p.groupRows) != 2 {
		t.Fatalf("groupRows = %+v", p.groupRows)
	}
	if p.groupRows[0].label != "workstream=hd" || p.groupRows[1].label != "(untagged)" {
		t.Fatalf("labels = %q / %q", p.groupRows[0].label, p.groupRows[1].label)
	}
	// cycling from tag:workstream wraps to flat and turns tree off
	p.treeMode = true
	p.cycleGroupBy()
	if p.groupBy != "" || p.treeMode {
		t.Fatalf("cycle wrap: groupBy=%q tree=%v", p.groupBy, p.treeMode)
	}
}

// hostPivotPicker mixes a local session with sessions carrying federation host
// labels, so the "host" pivot has three buckets: local, win01 (two sessions),
// and voltops-rcs.
func hostPivotPicker() *picker {
	now := time.Now()
	return &picker{
		all: []session.Session{
			{ID: "l1", Last: now},                                // local (Host == "")
			{ID: "w1", Host: "win01", Last: now.Add(-time.Hour)}, // win01
			{ID: "r1", Host: "voltops-rcs", Last: now.Add(-2 * time.Hour)},
			{ID: "w2", Host: "win01", Last: now.Add(-3 * time.Hour)}, // win01
		},
		meta:      map[string]view.RowMeta{},
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		km:        keys.Build(nil),
	}
}

// TestGroupByHost buckets sessions by their federation host: local sessions
// under a "local" group, remote sessions under their host name. Group headers
// carry the host names, ordered by most recent activity.
func TestGroupByHost(t *testing.T) {
	p := hostPivotPicker()
	p.groupBy = "host"
	p.recompute()

	// three buckets, ordered by latest activity: local, win01, voltops-rcs
	if len(p.groupRows) != 3 {
		t.Fatalf("groupRows = %+v", p.groupRows)
	}
	if p.groupRows[0].label != "local" || p.groupRows[1].label != "win01" || p.groupRows[2].label != "voltops-rcs" {
		t.Fatalf("labels = %q / %q / %q", p.groupRows[0].label, p.groupRows[1].label, p.groupRows[2].label)
	}
	// win01 bucketed both of its sessions; local and voltops-rcs one each.
	if p.groupRows[0].count != 1 || p.groupRows[1].count != 2 || p.groupRows[2].count != 1 {
		t.Fatalf("counts = %d / %d / %d", p.groupRows[0].count, p.groupRows[1].count, p.groupRows[2].count)
	}
	// header renders the host name with the fold arrow.
	line := view.StripANSI(p.headerLine(p.groupRows[1], -2, 120))
	if !strings.Contains(line, "▾ win01") {
		t.Fatalf("host header line: %q", line)
	}
}

// TestGroupByHostPersistence round-trips the host pivot (and a folded host
// group) through savePrefs/loadPrefs, and confirms B cycling reaches and
// leaves host.
func TestGroupByHostPersistence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	p := hostPivotPicker()
	p.groupBy = "host"
	p.recompute()
	p.cursor = 0 // the local header
	if !p.headerToggle() {
		t.Fatal("headerToggle should report a header")
	}

	saved := loadPrefs()
	if saved.GroupBy != "host" || len(saved.Collapsed) != 1 || saved.Collapsed[0] != "\x00local" {
		t.Fatalf("host pivot + fold should persist: %+v", saved)
	}

	// A fresh picker restoring under the same pivot sees the fold restored.
	p2 := hostPivotPicker()
	prefs := loadPrefs()
	p2.groupBy = prefs.GroupBy
	p2.collapsed = prefs.collapsedFor(p2.groupBy)
	if !p2.collapsed["\x00local"] {
		t.Fatalf("collapse should restore under the host pivot: %v", p2.collapsed)
	}

	// B cycling reaches host (run -> host) and leaves it (host -> tag).
	p3 := hostPivotPicker()
	p3.groupBy = "run"
	p3.recompute()
	p3.cycleGroupBy()
	if p3.groupBy != "host" {
		t.Fatalf("cycle run -> host, got %q", p3.groupBy)
	}
	p3.cycleGroupBy()
	if p3.groupBy == "host" {
		t.Fatalf("cycle should leave host, still %q", p3.groupBy)
	}
}

// Collapse state persists across picker invocations, scoped to the pivot it
// was collapsed under: a fold saved under "dir" must not survive a switch to
// a different GroupBy, and must come back once "dir" is active again.
func TestGroupByCollapsePersistence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	p := pivotPicker()
	p.groupBy = "dir"
	p.recompute()
	p.cursor = 0 // the /src/blog header
	if !p.headerToggle() {
		t.Fatal("headerToggle should report a header")
	}

	saved := loadPrefs()
	if saved.GroupBy != "dir" || len(saved.Collapsed) != 1 || saved.Collapsed[0] != "/src/blog" {
		t.Fatalf("headerToggle should persist the collapse: %+v", saved)
	}

	// A fresh picker restoring under the same pivot sees the fold restored.
	p2 := pivotPicker()
	prefs := loadPrefs()
	p2.groupBy = prefs.GroupBy
	p2.collapsed = prefs.collapsedFor(p2.groupBy)
	if !p2.collapsed["/src/blog"] {
		t.Fatalf("collapse should restore under the matching pivot: %v", p2.collapsed)
	}

	// setCollapsedAll(false) clears in memory and persists the clear.
	p.setCollapsedAll(false)
	if saved := loadPrefs(); len(saved.Collapsed) != 0 {
		t.Fatalf("expand-all should persist an empty collapse set, got %v", saved.Collapsed)
	}

	// Switching pivot resets collapse and must not leak the old pivot's fold
	// state under the new pivot's name.
	p.cursor = 0
	p.headerToggle() // re-collapse /src/blog under "dir"
	p.cycleGroupBy() // dir -> run
	if len(p.collapsed) != 0 {
		t.Fatalf("cycleGroupBy should reset in-memory collapse, got %v", p.collapsed)
	}
	saved = loadPrefs()
	if saved.GroupBy != "run" || len(saved.Collapsed) != 0 {
		t.Fatalf("cycleGroupBy should persist an empty collapse set under the new pivot: %+v", saved)
	}
	if set := saved.collapsedFor("dir"); len(set) != 0 {
		t.Fatalf("the old pivot's fold state must not remain reachable: %v", set)
	}
}
