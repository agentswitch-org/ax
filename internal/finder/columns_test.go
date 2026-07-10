package finder

import (
	"reflect"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/models"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

func colTestPicker(cols []string) *picker {
	return &picker{cfg: config.Config{Columns: cols}, km: keys.Build(nil)}
}

func boolPtr(b bool) *bool { return &b }

func colRowKey(rows []colRow, key string) (colRow, bool) {
	for _, r := range rows {
		if r.key == key {
			return r, true
		}
	}
	return colRow{}, false
}

// The modal lists every registry column, marking the ones currently in the layout
// visible and the rest hidden.
func TestCurrentColRowsCoversEveryColumn(t *testing.T) {
	p := colTestPicker([]string{"harness", "model", "title"})
	rows := p.currentColRows()
	if len(rows) < len(view.AllColumns(p.cfg)) {
		t.Fatalf("rows = %d, want >= %d", len(rows), len(view.AllColumns(p.cfg)))
	}
	for _, k := range []string{"harness", "model", "title"} {
		r, ok := colRowKey(rows, k)
		if !ok || !r.visible {
			t.Fatalf("%s should be visible, got %+v (ok=%v)", k, r, ok)
		}
	}
	dir, ok := colRowKey(rows, "dir")
	if !ok || dir.visible {
		t.Fatalf("dir should be listed but hidden, got %+v (ok=%v)", dir, ok)
	}
}

// Toggling a column off drops its header/row cell; toggling it back on restores it.
func TestColumnToggleHidesColumn(t *testing.T) {
	p := colTestPicker([]string{"harness", "model", "title"})
	rows := p.currentColRows()
	for i := range rows {
		if rows[i].key == "model" {
			rows[i].visible = false
		}
	}
	p.setColLayout(rows)

	header := view.StripANSI(view.Columns(p.cfg, 0, 0, false))
	if strings.Contains(header, "MODEL") {
		t.Fatalf("MODEL still shown after hide: %q", header)
	}
	if !strings.Contains(header, "HARNESS") || !strings.Contains(header, "TITLE") {
		t.Fatalf("expected harness+title still shown: %q", header)
	}

	for i := range rows {
		if rows[i].key == "model" {
			rows[i].visible = true
		}
	}
	p.setColLayout(rows)
	if header = view.StripANSI(view.Columns(p.cfg, 0, 0, false)); !strings.Contains(header, "MODEL") {
		t.Fatalf("MODEL not restored after re-enable: %q", header)
	}
}

// Reordering rows changes the horizontal order the header renders in.
func TestColumnReorderChangesOrder(t *testing.T) {
	p := colTestPicker([]string{"harness", "model", "title"})
	rows := p.currentColRows()
	rows[0], rows[1] = rows[1], rows[0] // model now precedes harness
	p.setColLayout(rows)

	if p.cfg.Columns[0] != "model" || p.cfg.Columns[1] != "harness" {
		t.Fatalf("order not applied: %v", p.cfg.Columns[:2])
	}
	header := view.StripANSI(view.Columns(p.cfg, 0, 0, false))
	mi, hi := strings.Index(header, "MODEL"), strings.Index(header, "HARNESS")
	if mi < 0 || hi < 0 || mi > hi {
		t.Fatalf("MODEL should precede HARNESS: %q", header)
	}
}

// Widening a (non-last) column grows the rendered header by exactly the delta;
// widths are clamped to the sane bounds.
func TestColumnWidthAdjustsRenderAndClamps(t *testing.T) {
	p := colTestPicker([]string{"model", "harness", "title"})
	base := view.StripANSI(view.Columns(p.cfg, 0, 0, false))

	rows := p.currentColRows()
	rows[0].width = clampColW(rows[0].width + colWidthStep) // widen model
	p.setColLayout(rows)
	wide := view.StripANSI(view.Columns(p.cfg, 0, 0, false))
	if len(wide) != len(base)+colWidthStep {
		t.Fatalf("header width delta = %d, want %d", len(wide)-len(base), colWidthStep)
	}

	if clampColW(colWidthMax+100) != colWidthMax {
		t.Fatalf("over-max not clamped: %d", clampColW(colWidthMax+100))
	}
	if clampColW(colWidthMin-100) != colWidthMin {
		t.Fatalf("under-min not clamped: %d", clampColW(colWidthMin-100))
	}
}

// The saved layout round-trips through savePrefs/loadPrefs, and an unrelated
// prefs write (a scope/group-by toggle) does not drop it.
func TestColumnLayoutRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	want := []colPref{{"harness", true, 10}, {"model", false, 14}, {"title", true, 40}}
	savePrefs(uiPrefs{Scope: int(scopeLive), GroupBy: "dir", Columns: want})
	got := loadPrefs()
	if !reflect.DeepEqual(got.Columns, want) {
		t.Fatalf("columns round-trip: got %+v, want %+v", got.Columns, want)
	}

	// A scope toggle re-saves via saveCurrentPrefs; the column layout must survive.
	p := &picker{colPrefs: got.Columns, collapsed: map[string]bool{}, scope: scopeWorking}
	p.saveCurrentPrefs()
	if reloaded := loadPrefs(); !reflect.DeepEqual(reloaded.Columns, want) {
		t.Fatalf("columns dropped by saveCurrentPrefs: got %+v, want %+v", reloaded.Columns, want)
	}
}

// applyColRows persists the modal layout and re-applies it to the live cfg.
func TestApplyColRowsPersistsAndApplies(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	p := &picker{
		cfg:       config.Config{Columns: []string{"harness", "model", "title"}},
		km:        keys.Build(nil),
		collapsed: map[string]bool{},
		marks:     map[string]bool{},
		visual:    -1,
	}
	p.recompute()
	rows := p.currentColRows()
	for i := range rows {
		if rows[i].key == "harness" {
			rows[i].visible = false
		}
	}
	p.applyColRows(rows)

	if strings.Contains(view.StripANSI(view.Columns(p.cfg, 0, 0, false)), "HARNESS") {
		t.Fatalf("harness not hidden after apply: %v", p.cfg.Columns)
	}
	saved := loadPrefs()
	h, ok := colRowKey(prefsToRows(saved.Columns), "harness")
	if !ok || h.visible {
		t.Fatalf("harness should be persisted hidden, got %+v (ok=%v)", h, ok)
	}
}

func prefsToRows(ps []colPref) []colRow {
	out := make([]colRow, len(ps))
	for i, p := range ps {
		out[i] = colRow{key: p.Key, visible: p.Visible, width: p.Width}
	}
	return out
}

// Config [[column]] defaults set default width/visibility/order, and reset
// (defaultColRows) reverts to exactly them.
func TestColumnConfigDefaultsAndReset(t *testing.T) {
	p := &picker{
		cfg: config.Config{
			Columns: []string{"harness", "model", "title"},
			ColumnDefaults: []config.ColumnDefault{
				{Key: "title", Width: 40},
				{Key: "id", Visible: boolPtr(true)},     // id is not default-visible; config shows it
				{Key: "model", Visible: boolPtr(false)}, // hide an otherwise-default column
			},
		},
		km: keys.Build(nil),
	}
	rows := p.defaultColRows()

	if rows[0].key != "title" || rows[0].width != 40 {
		t.Fatalf("title should lead at width 40, got %+v", rows[0])
	}
	if id, ok := colRowKey(rows, "id"); !ok || !id.visible {
		t.Fatalf("id should be config-visible, got %+v (ok=%v)", id, ok)
	}
	if m, ok := colRowKey(rows, "model"); !ok || m.visible {
		t.Fatalf("model should be config-hidden, got %+v (ok=%v)", m, ok)
	}

	p.setColLayout(rows) // simulate reset
	if p.cfg.Columns[0] != "title" {
		t.Fatalf("reset order not applied: %v", p.cfg.Columns)
	}
	header := view.StripANSI(view.Columns(p.cfg, 0, 0, false))
	if !strings.Contains(header, "ID") {
		t.Fatalf("config-visible id missing after reset: %q", header)
	}
	if strings.Contains(header, "MODEL") {
		t.Fatalf("config-hidden model shown after reset: %q", header)
	}
}

// The persisted layout is re-applied over a freshly loaded, auto-augmented cfg,
// so a reindex/relaunch keeps the user's chosen visible set and widths.
func TestApplySavedColLayoutReappliesOverReload(t *testing.T) {
	p := &picker{
		cfg: config.Config{Columns: []string{"harness", "model", "title"}},
		km:  keys.Build(nil),
	}
	// A full snapshot, exactly as the modal's OK writes it: title, harness visible;
	// everything else (including model) hidden.
	full := p.currentColRows()
	full[0], full[1] = full[1], full[0] // model, harness, title, ...
	var saved []colPref
	for _, r := range full {
		vis := r.key == "harness" || r.key == "title"
		w := r.width
		if r.key == "title" {
			w = 40
		}
		saved = append(saved, colPref{Key: r.key, Visible: vis, Width: w})
	}
	// Put title first in the saved order.
	for i, sp := range saved {
		if sp.Key == "title" {
			saved = append([]colPref{sp}, append(saved[:i:i], saved[i+1:]...)...)
			break
		}
	}
	p.colPrefs = saved
	p.applySavedColLayout()
	if got := p.cfg.Columns; len(got) != 2 || got[0] != "title" || got[1] != "harness" {
		t.Fatalf("saved layout not re-applied: %v", got)
	}
	if p.cfg.ColWidths["title"] != 40 {
		t.Fatalf("saved width not applied: %d", p.cfg.ColWidths["title"])
	}
	header := view.StripANSI(view.Columns(p.cfg, 0, 0, false))
	if strings.Contains(header, "MODEL") {
		t.Fatalf("model should stay hidden after reload: %q", header)
	}

	// A row still renders cleanly under the re-applied layout.
	row := view.StripANSI(view.Row(p.cfg, models.DB{},
		session.Session{Harness: "claude", Title: "hello"}, view.RowMeta{State: view.StateLive}, 0))
	if !strings.Contains(row, "hello") {
		t.Fatalf("row missing title cell: %q", row)
	}
}

// With neither a saved layout nor config defaults, applySavedColLayout leaves the
// cfg untouched (default behavior is preserved).
func TestApplySavedColLayoutNoopWithoutPrefs(t *testing.T) {
	p := colTestPicker([]string{"harness", "model", "title"})
	p.applySavedColLayout()
	if !reflect.DeepEqual(p.cfg.Columns, []string{"harness", "model", "title"}) {
		t.Fatalf("cfg.Columns mutated by no-op: %v", p.cfg.Columns)
	}
	if p.cfg.ColWidths != nil {
		t.Fatalf("ColWidths set by no-op: %v", p.cfg.ColWidths)
	}
}
