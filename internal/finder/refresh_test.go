package finder

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/retention"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

func newTestPicker(sessions []session.Session, meta map[string]view.RowMeta) *picker {
	p := &picker{
		cfg:       config.Config{Columns: []string{"status", "name", "title"}},
		all:       sessions,
		meta:      meta,
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		visual:    -1,
		km:        keys.Build(nil),
	}
	p.recompute()
	return p
}

func testMatchIDs(p *picker) []string {
	var out []string
	for mi := range p.matches {
		if s, ok := p.rowSession(mi); ok {
			out = append(out, s.ID)
		}
	}
	return out
}

// The `t` scope cycles All -> Live -> Working -> Active Run -> All, and each
// scope hides the right rows.
func TestScopeCycleThreeStates(t *testing.T) {
	sessions := []session.Session{
		{ID: "work"},
		{ID: "idle"},
		{ID: "dead"},
	}
	meta := map[string]view.RowMeta{
		"work": {State: view.StateLive, Activity: view.Working},
		"idle": {State: view.StateLive, Activity: view.Idle},
		"dead": {}, // inactive: no live state
	}
	p := newTestPicker(sessions, meta)

	ids := func() []string {
		var out []string
		for _, mi := range p.matches {
			if s, ok := p.rowSession(mi); ok {
				out = append(out, s.ID)
			}
		}
		return out
	}

	if p.scope != scopeAll {
		t.Fatalf("default scope should be All, got %v", p.scope)
	}
	if got := ids(); len(got) != 3 {
		t.Fatalf("scopeAll should show all 3 rows, got %v", got)
	}

	// t -> Live
	p.dispatch(keys.Scope)
	if p.scope != scopeLive {
		t.Fatalf("after one t, scope should be Live, got %v", p.scope)
	}
	if got := ids(); len(got) != 2 || got[0] != "work" || got[1] != "idle" {
		t.Fatalf("scopeLive should keep working+idle, drop inactive; got %v", got)
	}

	// t -> Working
	p.dispatch(keys.Scope)
	if p.scope != scopeWorking {
		t.Fatalf("after two t, scope should be Working, got %v", p.scope)
	}
	if got := ids(); len(got) != 1 || got[0] != "work" {
		t.Fatalf("scopeWorking should keep only working, drop idle+inactive; got %v", got)
	}

	// t -> Active Run
	p.dispatch(keys.Scope)
	if p.scope != scopeActiveRun {
		t.Fatalf("after three t, scope should be Active Run, got %v", p.scope)
	}
	if got := ids(); len(got) != 1 || got[0] != "work" {
		t.Fatalf("scopeActiveRun should keep working rows here; got %v", got)
	}

	// t -> back to All
	p.dispatch(keys.Scope)
	if p.scope != scopeAll {
		t.Fatalf("after four t, scope should wrap to All, got %v", p.scope)
	}
	if got := ids(); len(got) != 3 {
		t.Fatalf("scopeAll should show all 3 again, got %v", got)
	}
}

func TestMetaRefreshRecomputesAllScopeInsertFilter(t *testing.T) {
	sessions := []session.Session{
		{ID: "left", Name: "left"},
		{ID: "right", Name: "right"},
	}

	for _, tc := range []struct {
		name      string
		committed bool
	}{
		{name: "open filter"},
		{name: "committed filter", committed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPicker(sessions, map[string]view.RowMeta{
				"left":  {State: view.StateLive, Activity: view.Working},
				"right": {State: view.StateLive, Activity: view.Idle},
			})
			p.cfg.Columns = []string{"activity", "name"}
			p.selCol = 0
			p.recompute()

			typeFilter(p, "working")
			if got := testMatchIDs(p); len(got) != 1 || got[0] != "left" {
				t.Fatalf("initial activity filter = %v, want left", got)
			}
			if tc.committed {
				p.handleInsert(ev{t: evEnter})
				if p.mode != mNormal || p.committed != mFilter {
					t.Fatalf("filter did not commit: mode=%d committed=%d", p.mode, p.committed)
				}
			}
			if p.scope != scopeAll {
				t.Fatalf("test requires All scope, got %v", p.scope)
			}

			p.applyMetaReady(map[string]view.RowMeta{
				"left":  {State: view.StateLive, Activity: view.Idle},
				"right": {State: view.StateLive, Activity: view.Working},
			})
			if got := testMatchIDs(p); len(got) != 1 || got[0] != "right" {
				t.Fatalf("activity filter after metadata refresh = %v, want right", got)
			}
		})
	}
}

func TestCommittedFilterRefreshKeepsCursorOnHighlightedSession(t *testing.T) {
	sessions := []session.Session{
		{ID: "left", Name: "left"},
		{ID: "picked", Name: "picked"},
		{ID: "right", Name: "right"},
	}
	p := newTestPicker(sessions, map[string]view.RowMeta{
		"left":   {State: view.StateLive, Activity: view.Working},
		"picked": {State: view.StateLive, Activity: view.Working},
		"right":  {State: view.StateLive, Activity: view.Idle},
	})
	p.cfg.Columns = []string{"activity", "name"}
	p.selCol = 0
	p.recompute()
	typeFilter(p, "working")
	p.handleInsert(ev{t: evEnter})
	p.cursor = 1

	p.applyMetaReady(map[string]view.RowMeta{
		"left":   {State: view.StateLive, Activity: view.Idle},
		"picked": {State: view.StateLive, Activity: view.Working},
		"right":  {State: view.StateLive, Activity: view.Working},
	})

	if s, ok := p.cur(); !ok || s.ID != "picked" {
		t.Fatalf("cursor after filtered refresh = ok=%v session=%+v, want picked", ok, s)
	}
	if !p.open() {
		t.Fatal("Enter did not pick the highlighted session")
	}
	if got := p.choice.Picked; len(got) != 1 || got[0].ID != "picked" {
		t.Fatalf("picked after filtered refresh = %+v, want picked", got)
	}
}

func TestSortKeepsCursorOnHighlightedSession(t *testing.T) {
	p := newTestPicker([]session.Session{
		{ID: "picked", Name: "picked", Cost: 10, HasCost: true},
		{ID: "rich", Name: "rich", Cost: 100, HasCost: true},
		{ID: "cheap", Name: "cheap", Cost: 1, HasCost: true},
	}, map[string]view.RowMeta{})
	p.cfg.Columns = []string{"cost", "name"}
	p.selCol = 0
	p.cursor = 0

	p.sortBy(0)

	if s, ok := p.cur(); !ok || s.ID != "picked" {
		t.Fatalf("cursor after sort = ok=%v session=%+v, want picked", ok, s)
	}
}

func TestArchiveFilterCycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	p := newTestPicker([]session.Session{
		{ID: "active"},
		{ID: "archived", Archived: true},
	}, map[string]view.RowMeta{})

	ids := func() []string {
		var out []string
		for mi := range p.matches {
			if s, ok := p.rowSession(mi); ok {
				out = append(out, s.ID)
			}
		}
		return out
	}
	if got := ids(); len(got) != 1 || got[0] != "active" {
		t.Fatalf("default archive filter should show active only, got %v", got)
	}
	p.dispatch(keys.Archive)
	if p.archive != retention.All {
		t.Fatalf("after one archive cycle, got %v", p.archive)
	}
	if got := ids(); len(got) != 2 {
		t.Fatalf("all archive filter should show both rows, got %v", got)
	}
	p.dispatch(keys.Archive)
	if p.archive != retention.ArchivedOnly {
		t.Fatalf("after two archive cycles, got %v", p.archive)
	}
	if got := ids(); len(got) != 1 || got[0] != "archived" {
		t.Fatalf("archived-only filter should show archived row, got %v", got)
	}
}

func TestArchiveStatusSegmentLabelsEveryMode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := newTestPicker([]session.Session{{ID: "active"}}, map[string]view.RowMeta{})

	if got := view.StripANSI(p.statusSegment()); got != "archive:unarchived" {
		t.Fatalf("default archive status = %q, want archive:unarchived", got)
	}
	p.dispatch(keys.Archive)
	if got := view.StripANSI(p.statusSegment()); got != "archive:all" {
		t.Fatalf("all archive status = %q, want archive:all", got)
	}
	p.dispatch(keys.Archive)
	if got := view.StripANSI(p.statusSegment()); got != "archive:archived" {
		t.Fatalf("archived archive status = %q, want archive:archived", got)
	}
}

func TestToggleArchivedArchivesSelectionAndHidesFromActiveView(t *testing.T) {
	p := newTestPicker([]session.Session{{ID: "active"}}, map[string]view.RowMeta{"active": {}})
	var confirmed bool
	var got []ArchiveChange
	p.confirmFn = func(msg string) bool {
		confirmed = true
		if want := "archive 1 session(s)? hides from default view"; msg != want {
			t.Fatalf("confirm message = %q, want %q", msg, want)
		}
		return true
	}
	p.onArchive = func(changes []ArchiveChange) map[string]error {
		got = append(got, changes...)
		return nil
	}

	p.dispatch(keys.ToggleArchived)
	if !confirmed {
		t.Fatal("archive action did not ask for confirmation")
	}
	if len(got) != 1 || got[0].Session.ID != "active" || !got[0].Archived {
		t.Fatalf("archive changes = %+v, want active -> archived", got)
	}
	if !p.all[0].Archived || !p.meta["active"].Archived {
		t.Fatalf("successful archive did not update picker state: session=%+v meta=%+v", p.all[0], p.meta["active"])
	}
	if ids := testMatchIDs(p); len(ids) != 0 {
		t.Fatalf("archived row should leave active-only view, got %v", ids)
	}
	p.archive = retention.ArchivedOnly
	p.recompute()
	if ids := testMatchIDs(p); len(ids) != 1 || ids[0] != "active" {
		t.Fatalf("archived row should appear in archived view, got %v", ids)
	}
}

func TestToggleArchivedConfirmMessageCountsLiveSessions(t *testing.T) {
	p := newTestPicker([]session.Session{
		{ID: "live"},
		{ID: "idle"},
	}, map[string]view.RowMeta{
		"live": {State: view.StateLive},
		"idle": {},
	})
	p.toggleMark()
	p.toggleMark()
	var msg string
	p.confirmFn = func(m string) bool {
		msg = m
		return false
	}
	p.onArchive = func(changes []ArchiveChange) map[string]error {
		t.Fatal("onArchive should not run when confirm is declined")
		return nil
	}

	p.dispatch(keys.ToggleArchived)
	if want := "archive 2 session(s)? hides from default view (1 live)"; msg != want {
		t.Fatalf("confirm message = %q, want %q", msg, want)
	}
}

func TestToggleArchivedArchivesVisualSelectionAndHidesFromActiveView(t *testing.T) {
	sessions := make([]session.Session, 8)
	meta := map[string]view.RowMeta{}
	for i := range sessions {
		id := fmt.Sprintf("session-%d", i+1)
		sessions[i] = session.Session{ID: id}
		meta[id] = view.RowMeta{}
	}
	p := newTestPicker(sessions, meta)
	p.confirmFn = func(string) bool { return true }
	var got []ArchiveChange
	p.onArchive = func(changes []ArchiveChange) map[string]error {
		got = append(got, changes...)
		return nil
	}

	p.dispatch(keys.Visual)
	p.move(6)
	p.dispatch(keys.ToggleArchived)

	if len(got) != 7 {
		t.Fatalf("visual archive changes = %d, want 7: %+v", len(got), got)
	}
	for i, ch := range got {
		wantID := fmt.Sprintf("session-%d", i+1)
		if ch.Session.ID != wantID || !ch.Archived {
			t.Fatalf("visual archive change %d = %+v, want %s archived", i, ch, wantID)
		}
	}
	if ids := testMatchIDs(p); len(ids) != 1 || ids[0] != "session-8" {
		t.Fatalf("visual archived rows should leave active-only view, got %v", ids)
	}
	if s, ok := p.cur(); !ok || s.ID != "session-8" {
		t.Fatalf("cursor after visual archive = ok=%v session=%+v, want session-8", ok, s)
	}
	if len(p.marks) != 0 || p.visual != -1 {
		t.Fatalf("selection should clear after visual archive: marks=%v visual=%d", p.marks, p.visual)
	}
}

func TestToggleArchivedHidesRowAndMovesSelectionToVisibleSession(t *testing.T) {
	p := newTestPicker([]session.Session{
		{ID: "first"},
		{ID: "second"},
		{ID: "third"},
	}, map[string]view.RowMeta{"first": {}, "second": {}, "third": {}})
	p.cursor = 1
	p.confirmFn = func(string) bool { return true }
	p.onArchive = func(changes []ArchiveChange) map[string]error { return nil }

	p.dispatch(keys.ToggleArchived)

	ids := testMatchIDs(p)
	if len(ids) != 2 || ids[0] != "first" || ids[1] != "third" {
		t.Fatalf("archived row should leave unarchived view immediately, got %v", ids)
	}
	if s, ok := p.cur(); !ok || s.ID != "third" {
		t.Fatalf("cursor should move to a visible remaining row, got ok=%v session=%+v", ok, s)
	}
	if !p.all[1].Archived || !p.meta["second"].Archived {
		t.Fatalf("archive state mismatch after success: session=%+v meta=%+v", p.all[1], p.meta["second"])
	}
}

func TestToggleArchivedUnarchivesSelectionAndRestoresActiveView(t *testing.T) {
	p := newTestPicker(
		[]session.Session{{ID: "restored", Archived: true}},
		map[string]view.RowMeta{"restored": {Archived: true}},
	)
	p.archive = retention.ArchivedOnly
	p.recompute()
	if ids := testMatchIDs(p); len(ids) != 1 || ids[0] != "restored" {
		t.Fatalf("archived-only view should start on archived row, got %v", ids)
	}
	var got []ArchiveChange
	p.confirmFn = func(msg string) bool {
		if want := "unarchive 1 session(s)? restores to default view"; msg != want {
			t.Fatalf("confirm message = %q, want %q", msg, want)
		}
		return true
	}
	p.onArchive = func(changes []ArchiveChange) map[string]error {
		got = append(got, changes...)
		return nil
	}

	p.dispatch(keys.ToggleArchived)
	if len(got) != 1 || got[0].Session.ID != "restored" || got[0].Archived {
		t.Fatalf("archive changes = %+v, want restored -> unarchived", got)
	}
	if p.all[0].Archived || p.meta["restored"].Archived {
		t.Fatalf("successful unarchive did not update picker state: session=%+v meta=%+v", p.all[0], p.meta["restored"])
	}
	if ids := testMatchIDs(p); len(ids) != 0 {
		t.Fatalf("unarchived row should leave archived-only view, got %v", ids)
	}
	p.archive = retention.ActiveOnly
	p.recompute()
	if ids := testMatchIDs(p); len(ids) != 1 || ids[0] != "restored" {
		t.Fatalf("unarchived row should return to active-only view, got %v", ids)
	}
}

func TestToggleArchivedFailedArchiveKeepsRowVisible(t *testing.T) {
	p := newTestPicker([]session.Session{{ID: "live"}}, map[string]view.RowMeta{"live": {State: view.StateLive}})
	p.confirmFn = func(string) bool { return true }
	p.onArchive = func([]ArchiveChange) map[string]error {
		return map[string]error{"live": errors.New("live session requires --force")}
	}

	p.dispatch(keys.ToggleArchived)
	if p.all[0].Archived || p.meta["live"].Archived {
		t.Fatalf("failed archive changed picker state: session=%+v meta=%+v", p.all[0], p.meta["live"])
	}
	if ids := testMatchIDs(p); len(ids) != 1 || ids[0] != "live" {
		t.Fatalf("failed archive should keep row in active view, got %v", ids)
	}
	if got := view.StripANSI(p.notice); !strings.Contains(got, "failed 1: live session requires --force") {
		t.Fatalf("notice = %q, want failure detail", got)
	}
}

func TestToggleArchivedMixedFailureKeepsFailedRowsVisible(t *testing.T) {
	p := newTestPicker([]session.Session{
		{ID: "live"},
		{ID: "done"},
	}, map[string]view.RowMeta{"live": {State: view.StateLive}, "done": {}})
	p.marks["live"] = true
	p.marks["done"] = true
	p.confirmFn = func(string) bool { return true }
	p.onArchive = func([]ArchiveChange) map[string]error {
		return map[string]error{"live": errors.New("live session requires --force")}
	}

	p.dispatch(keys.ToggleArchived)

	if ids := testMatchIDs(p); len(ids) != 1 || ids[0] != "live" {
		t.Fatalf("only the failed row should remain visible, got %v", ids)
	}
	if p.all[0].Archived {
		t.Fatalf("failed live row should not be archived: %+v", p.all[0])
	}
	if !p.all[1].Archived {
		t.Fatalf("successful done row should be archived: %+v", p.all[1])
	}
	got := view.StripANSI(p.notice)
	if !strings.Contains(got, "archived 1 session") || !strings.Contains(got, "failed 1: live session requires --force") {
		t.Fatalf("notice = %q, want success count and failure detail", got)
	}
}

func TestToggleArchivedSurvivesStaleReindex(t *testing.T) {
	p := newTestPicker([]session.Session{
		{ID: "first"},
		{ID: "second"},
		{ID: "third"},
	}, map[string]view.RowMeta{"first": {}, "second": {}, "third": {}})
	p.confirmFn = func(string) bool { return true }
	p.onArchive = func([]ArchiveChange) map[string]error { return nil }

	p.dispatch(keys.Visual)
	p.move(1)
	p.dispatch(keys.ToggleArchived)
	if ids := testMatchIDs(p); len(ids) != 1 || ids[0] != "third" {
		t.Fatalf("archived rows should hide before reindex, got %v", ids)
	}

	p.applyReindex(reindexResult{
		sessions: []session.Session{{ID: "first"}, {ID: "second"}, {ID: "third"}},
		cfg:      p.cfg,
	})

	if ids := testMatchIDs(p); len(ids) != 1 || ids[0] != "third" {
		t.Fatalf("stale reindex resurrected archived rows, got %v", ids)
	}
	if !p.all[0].Archived || !p.all[1].Archived || p.all[2].Archived {
		t.Fatalf("archive overlay after stale reindex = %+v", p.all)
	}
}

func TestToggleArchivedBulkSelectionBatchesStateMutation(t *testing.T) {
	const selected = 600
	const total = 750
	sessions := make([]session.Session, total)
	meta := make(map[string]view.RowMeta, total)
	for i := range sessions {
		id := fmt.Sprintf("bulk-%04d", i)
		sessions[i] = session.Session{ID: id}
		meta[id] = view.RowMeta{}
	}
	p := newTestPicker(sessions, meta)
	for i := 0; i < selected; i++ {
		p.marks[sessions[i].ID] = true
	}
	p.confirmFn = func(string) bool { return true }
	var calls int
	p.onArchive = func(changes []ArchiveChange) map[string]error {
		calls++
		if len(changes) != selected {
			t.Fatalf("archive callback got %d changes, want %d", len(changes), selected)
		}
		return nil
	}

	p.dispatch(keys.ToggleArchived)

	if calls != 1 {
		t.Fatalf("archive callback calls = %d, want 1", calls)
	}
	if ids := testMatchIDs(p); len(ids) != total-selected {
		t.Fatalf("visible rows after bulk archive = %d, want %d", len(ids), total-selected)
	}
	for i := 0; i < selected; i++ {
		if !p.all[i].Archived {
			t.Fatalf("selected row %d was not archived: %+v", i, p.all[i])
		}
	}
	if len(p.marks) != 0 || p.visual != -1 {
		t.Fatalf("bulk archive should clear selection: marks=%d visual=%d", len(p.marks), p.visual)
	}
}

// The persisted preference migrates the legacy activeOnly toggle to scopeLive and
// otherwise round-trips the scope value.
func TestPrefsScopeMigration(t *testing.T) {
	if got := (uiPrefs{ActiveOnly: true}).scope(); got != scopeLive {
		t.Fatalf("legacy activeOnly=true should map to scopeLive, got %v", got)
	}
	if got := (uiPrefs{}).scope(); got != scopeAll {
		t.Fatalf("empty prefs should be scopeAll, got %v", got)
	}
	if got := (uiPrefs{Scope: int(scopeWorking)}).scope(); got != scopeWorking {
		t.Fatalf("stored scope should round-trip, got %v", got)
	}
}

func TestPrefsDefaultGroupByRun(t *testing.T) {
	if got := (uiPrefs{}).groupBy(); got != "run" {
		t.Fatalf("default groupBy = %q, want run", got)
	}
}

func TestLoadPrefsDefaultsAllScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	got := loadPrefs()
	if got.scope() != scopeAll {
		t.Fatalf("missing prefs default scope = %v, want all", got.scope())
	}
	if got.groupBy() != "run" {
		t.Fatalf("missing prefs default groupBy = %q, want run", got.groupBy())
	}
}

// GroupBy prefs round-trip: saving and loading preserves the pivot value
// alongside the scope, so the two fields do not clobber each other.
func TestPrefsGroupByRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	savePrefs(uiPrefs{Scope: int(scopeLive), GroupBy: "dir"})
	got := loadPrefs()
	if got.scope() != scopeLive {
		t.Fatalf("scope should survive GroupBy save, got %v", got.scope())
	}
	if got.GroupBy != "dir" {
		t.Fatalf("GroupBy should round-trip as 'dir', got %q", got.GroupBy)
	}

	// Saving scope alone must not wipe GroupBy.
	savePrefs(uiPrefs{Scope: int(scopeWorking), GroupBy: got.GroupBy})
	got2 := loadPrefs()
	if got2.GroupBy != "dir" {
		t.Fatalf("GroupBy should survive scope-only save, got %q", got2.GroupBy)
	}

	// Empty GroupBy serializes as omitempty and loads back as "".
	savePrefs(uiPrefs{Scope: int(scopeAll), GroupBy: ""})
	got3 := loadPrefs()
	if got3.GroupBy != "" {
		t.Fatalf("empty GroupBy should round-trip as empty string, got %q", got3.GroupBy)
	}
}

// Collapsed group keys round-trip alongside GroupBy, and only apply when the
// current pivot matches the one they were saved under: switching pivots must
// not leak a stale fold state into the new one.
func TestPrefsCollapsedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	savePrefs(uiPrefs{Scope: int(scopeLive), GroupBy: "dir", Collapsed: []string{"/src/blog", "/src/ax"}})
	got := loadPrefs()
	if got.GroupBy != "dir" {
		t.Fatalf("GroupBy should round-trip, got %q", got.GroupBy)
	}

	set := got.collapsedFor("dir")
	if len(set) != 2 || !set["/src/blog"] || !set["/src/ax"] {
		t.Fatalf("collapsedFor(matching pivot) = %v", set)
	}

	if set := got.collapsedFor("run"); len(set) != 0 {
		t.Fatalf("collapsedFor(different pivot) should be empty, got %v", set)
	}
}

// The i filter matches session IDs when the ID column is selected, so pasting an
// id (or a unique prefix) reliably lands on the right session without leaking ID
// matching into filters for other highlighted columns.
func TestFilterMatchesSessionID(t *testing.T) {
	sessions := []session.Session{
		{ID: "abc12345-0000-0000-0000-000000000000", Title: "alpha"},
		{ID: "def67890-0000-0000-0000-000000000000", Title: "beta"},
		{ID: "abc99999-0000-0000-0000-000000000000", Title: "gamma"},
	}
	p := newTestPicker(sessions, map[string]view.RowMeta{})
	p.cfg.Columns = []string{"id", "name", "title"}
	p.selCol = 0
	p.recompute()

	matchIDs := func(pp *picker) []string {
		var out []string
		for mi := range pp.matches { // range by index; rowSession expects a match-list index
			if s, ok := pp.rowSession(mi); ok {
				out = append(out, s.ID)
			}
		}
		return out
	}

	// Prefix of the first id only
	p.enter(mFilter)
	for _, r := range "abc12345" {
		p.handleInsert(ev{t: evRune, r: r})
	}
	got := matchIDs(p)
	if len(got) != 1 || got[0] != "abc12345-0000-0000-0000-000000000000" {
		t.Fatalf("id prefix filter should return exactly the matching session, got %v", got)
	}

	// "abc" prefix matches two sessions
	p.enter(mFilter)
	for _, r := range "abc" {
		p.handleInsert(ev{t: evRune, r: r})
	}
	got2 := matchIDs(p)
	if len(got2) != 2 {
		t.Fatalf("shared id prefix 'abc' should match 2 sessions, got %v", got2)
	}
}

// A live reindex brings in a newly created session, keeps the cursor on the same
// session (by id, not row index), and preserves the visual/mark selection.
func TestReindexAddsSessionPreservesCursor(t *testing.T) {
	p := newTestPicker([]session.Session{{ID: "a", Title: "alpha"}}, map[string]view.RowMeta{})
	// Cursor sits on "a".
	if s, ok := p.cur(); !ok || s.ID != "a" {
		t.Fatalf("cursor should start on a")
	}

	// Another shell creates session "b"; the reindex returns both, with b first
	// (as a fresh transcript would sort newest-first before an explicit sort).
	rr := reindexResult{
		sessions: []session.Session{{ID: "b", Title: "beta"}, {ID: "a", Title: "alpha"}},
		cfg:      p.cfg,
	}
	p.applyReindex(rr)

	seen := map[string]bool{}
	for _, mi := range p.matches {
		if s, ok := p.rowSession(mi); ok {
			seen[s.ID] = true
		}
	}
	if !seen["b"] {
		t.Fatalf("newly created session b should appear after reindex; matches=%v", p.matches)
	}
	if s, ok := p.cur(); !ok || s.ID != "a" {
		t.Fatalf("cursor should stay on session a after reindex, got %v", s.ID)
	}
}

func TestOpenUsesHighlightedSessionAfterReindexChurn(t *testing.T) {
	p := newTestPicker([]session.Session{
		{ID: "left", Title: "left"},
		{ID: "weyoun-6", Title: "target"},
		{ID: "right", Title: "right"},
	}, map[string]view.RowMeta{})
	p.cursor = 1

	p.applyReindex(reindexResult{sessions: []session.Session{
		{ID: "right", Title: "right"},
		{ID: "left", Title: "left"},
		{ID: "weyoun-6", Title: "target"},
	}, cfg: p.cfg})
	p.applyReindex(reindexResult{sessions: []session.Session{
		{ID: "left", Title: "left"},
		{ID: "weyoun-6", Title: "target"},
		{ID: "right", Title: "right"},
	}, cfg: p.cfg})

	if s, ok := p.cur(); !ok || s.ID != "weyoun-6" {
		t.Fatalf("cursor after churn = ok=%v session=%+v, want weyoun-6", ok, s)
	}
	if !p.open() {
		t.Fatal("Enter did not produce a choice")
	}
	if got := p.choice.Picked; len(got) != 1 || got[0].ID != "weyoun-6" {
		t.Fatalf("picked after churn = %+v, want weyoun-6", got)
	}
}

// A reindex that drops a session removes it from the view, and an empty scan is
// ignored rather than blanking the picker.
func TestReindexRemovesAndGuardsEmpty(t *testing.T) {
	p := newTestPicker([]session.Session{{ID: "a"}, {ID: "b"}}, map[string]view.RowMeta{})

	// b is killed elsewhere: reindex returns only a.
	p.applyReindex(reindexResult{sessions: []session.Session{{ID: "a"}}, cfg: p.cfg})
	for _, mi := range p.matches {
		if s, ok := p.rowSession(mi); ok && s.ID == "b" {
			t.Fatalf("removed session b should be gone after reindex")
		}
	}

	// A transient empty scan must not blank the list.
	before := len(p.matches)
	p.applyReindex(reindexResult{sessions: nil, cfg: p.cfg})
	if len(p.matches) != before {
		t.Fatalf("empty scan should be ignored, matches changed from %d to %d", before, len(p.matches))
	}
}
