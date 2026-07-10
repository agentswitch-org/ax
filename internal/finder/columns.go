package finder

import (
	"fmt"
	"strings"

	"github.com/agentswitch-org/ax/internal/view"
)

// Column-management modal: a picker overlay (default key `|`) that lists every
// column with a description and lets the user toggle visibility, reorder
// horizontally, and resize by a small interval, then OK to apply+persist or Esc
// to discard. The chosen layout lives in uiPrefs (ui.json) and is re-applied over
// the loaded, auto-augmented cfg on every (re)scan (applySavedColLayout), so it
// survives quit/resume. Per-column default width/visibility/order come from the
// config [[column]] tables (see config.ColumnDefault), which the modal's reset
// (r) reverts to.

const (
	colWidthMin  = 2  // narrowest a column may be shrunk to
	colWidthMax  = 60 // widest a column may be grown to
	colWidthStep = 2  // resize interval per +/- press
)

// colRow is one column's working state inside the modal.
type colRow struct {
	key     string
	label   string
	help    string
	visible bool
	width   int
}

// clampColW keeps a width inside the sane render bounds.
func clampColW(w int) int {
	return min(max(w, colWidthMin), colWidthMax)
}

// widthOf resolves a column's current width: the modal/runtime override if any,
// else the built-in registry default.
func (p *picker) widthOf(ci view.ColInfo) int {
	if w, ok := p.cfg.ColWidths[ci.Key]; ok && w > 0 {
		return clampColW(w)
	}
	return ci.Width
}

// currentColRows is the modal's initial state: the picker's live layout (visible
// columns first, in their current order and widths) followed by every other
// column shown as hidden. This mirrors exactly what is on screen, incorporating
// config columns, the auto-inserted columns, and any prior saved layout.
func (p *picker) currentColRows() []colRow {
	uni := view.AllColumns(p.cfg)
	info := make(map[string]view.ColInfo, len(uni))
	for _, ci := range uni {
		info[ci.Key] = ci
	}
	var rows []colRow
	seen := map[string]bool{}
	for _, k := range p.cfg.Columns {
		kk := view.ColKey(k)
		ci, ok := info[kk]
		if !ok || seen[kk] {
			continue
		}
		seen[kk] = true
		rows = append(rows, colRow{ci.Key, ci.Label, ci.Help, true, p.widthOf(ci)})
	}
	for _, ci := range uni {
		if seen[ci.Key] {
			continue
		}
		seen[ci.Key] = true
		rows = append(rows, colRow{ci.Key, ci.Label, ci.Help, false, p.widthOf(ci)})
	}
	return rows
}

// defaultColRows is the reset baseline: the config [[column]] defaults over the
// built-in registry defaults, independent of the current live layout. Columns
// named in config come first in config order; the rest follow in registry order.
func (p *picker) defaultColRows() []colRow {
	uni := view.AllColumns(p.cfg)
	info := make(map[string]view.ColInfo, len(uni))
	for _, ci := range uni {
		info[ci.Key] = ci
	}
	type ov struct {
		width   int
		visible *bool
	}
	cfgOv := map[string]ov{}
	var order []string
	for _, cd := range p.cfg.ColumnDefaults {
		k := view.ColKey(cd.Key)
		if k == "" {
			continue
		}
		if _, dup := cfgOv[k]; !dup {
			order = append(order, k)
		}
		cfgOv[k] = ov{cd.Width, cd.Visible}
	}
	visW := func(ci view.ColInfo) (bool, int) {
		vis, w := ci.DefaultVisible, ci.Width
		if o, ok := cfgOv[ci.Key]; ok {
			if o.visible != nil {
				vis = *o.visible
			}
			if o.width > 0 {
				w = clampColW(o.width)
			}
		}
		return vis, w
	}
	var rows []colRow
	seen := map[string]bool{}
	for _, k := range order {
		ci, ok := info[k]
		if !ok || seen[k] {
			continue
		}
		seen[k] = true
		vis, w := visW(ci)
		rows = append(rows, colRow{ci.Key, ci.Label, ci.Help, vis, w})
	}
	for _, ci := range uni {
		if seen[ci.Key] {
			continue
		}
		seen[ci.Key] = true
		vis, w := visW(ci)
		rows = append(rows, colRow{ci.Key, ci.Label, ci.Help, vis, w})
	}
	return rows
}

// savedColRows merges the persisted layout (p.colPrefs) over the current column
// universe: saved columns keep their order/visibility/width, and any column not
// in the save (a newly added registry or tag column) is appended at its built-in
// default. Used to re-apply the saved layout on every (re)scan.
func (p *picker) savedColRows() []colRow {
	uni := view.AllColumns(p.cfg)
	info := make(map[string]view.ColInfo, len(uni))
	for _, ci := range uni {
		info[ci.Key] = ci
	}
	var rows []colRow
	seen := map[string]bool{}
	for _, sp := range p.colPrefs {
		k := view.ColKey(sp.Key)
		ci, ok := info[k]
		if !ok || seen[k] {
			continue
		}
		seen[k] = true
		w := sp.Width
		if w <= 0 {
			w = ci.Width // legacy/hand-edited entry with no width
		}
		rows = append(rows, colRow{ci.Key, ci.Label, ci.Help, sp.Visible, clampColW(w)})
	}
	for _, ci := range uni {
		if seen[ci.Key] {
			continue
		}
		seen[ci.Key] = true
		rows = append(rows, colRow{ci.Key, ci.Label, ci.Help, ci.DefaultVisible, ci.Width})
	}
	return rows
}

// setColLayout projects modal rows onto the render config: the visible keys in
// order become cfg.Columns, and every row's width becomes a cfg.ColWidths
// override. It only rewrites the layout fields; callers recompute as needed.
func (p *picker) setColLayout(rows []colRow) {
	var cols []string
	widths := make(map[string]int, len(rows))
	for _, r := range rows {
		if r.visible {
			cols = append(cols, r.key)
		}
		widths[r.key] = clampColW(r.width)
	}
	p.cfg.Columns = cols
	p.cfg.ColWidths = widths
}

// applySavedColLayout re-applies the persisted column layout on top of the
// freshly loaded, auto-augmented cfg, so the user's chosen visible set, order,
// and widths survive a reindex and a relaunch. No-op when there is neither a
// saved layout nor config column defaults, preserving the default behavior for
// users who never touch the modal.
func (p *picker) applySavedColLayout() {
	if len(p.colPrefs) == 0 && len(p.cfg.ColumnDefaults) == 0 {
		return
	}
	if len(p.colPrefs) > 0 {
		p.setColLayout(p.savedColRows())
	} else {
		p.setColLayout(p.defaultColRows())
	}
}

// applyColRows is the modal's OK: persist the layout to ui.json and apply it to
// the live picker, re-sorting and re-rendering so the new visible set, order, and
// widths take effect immediately.
func (p *picker) applyColRows(rows []colRow) {
	p.colPrefs = p.colPrefs[:0]
	for _, r := range rows {
		p.colPrefs = append(p.colPrefs, colPref{Key: r.key, Visible: r.visible, Width: clampColW(r.width)})
	}
	p.saveCurrentPrefs()
	p.setColLayout(rows)
	if n := view.NumCols(p.cfg); n > 0 { // keep the sort/H-L cursor on a real column
		if p.sortCol >= n {
			p.sortCol = view.DefaultSortCol(p.cfg)
			p.sortDesc = view.DefaultDescFor(p.cfg, p.sortCol)
		}
		if p.selCol >= n {
			p.selCol = p.sortCol
		}
	}
	p.rowText = map[string]string{}
	p.applySort()
	p.recompute()
	p.previewDirty = true
}

// columnEditor runs the column-management modal, blocking the picker while open.
// Esc/q discards; Enter applies and persists. See the package comment.
func (p *picker) columnEditor() {
	rows := p.currentColRows()
	cur, top := 0, 0
	body := func() int {
		if h := p.sc.rows - 8; h > 3 {
			return h
		}
		return 3
	}
	clampView := func() {
		if cur < 0 {
			cur = 0
		}
		if cur >= len(rows) {
			cur = len(rows) - 1
		}
		if cur < top {
			top = cur
		}
		if b := body(); cur >= top+b {
			top = cur - b + 1
		}
		if top < 0 {
			top = 0
		}
	}
	for {
		clampView()
		p.sc.render(p.overlay(p.colBox(rows, cur, top)))
		e := <-p.sc.events
		switch e.t {
		case evEsc, evCtrlC:
			return // cancel: discard
		case evEnter:
			p.applyColRows(rows)
			return
		case evUp:
			cur--
		case evDown:
			cur++
		case evRune:
			switch e.r {
			case 'q':
				return
			case 'k':
				cur--
			case 'j':
				cur++
			case ' ':
				rows[cur].visible = !rows[cur].visible
			case '[':
				if cur > 0 {
					rows[cur-1], rows[cur] = rows[cur], rows[cur-1]
					cur--
				}
			case ']':
				if cur < len(rows)-1 {
					rows[cur+1], rows[cur] = rows[cur], rows[cur+1]
					cur++
				}
			case '+', '>', '=':
				rows[cur].width = clampColW(rows[cur].width + colWidthStep)
			case '-', '<', '_':
				rows[cur].width = clampColW(rows[cur].width - colWidthStep)
			case 'r':
				rows = p.defaultColRows()
				cur, top = 0, 0
			}
		}
	}
}

// colBox renders the modal: a titled box listing the (scrolled) column rows and a
// two-line footer of the controls.
func (p *picker) colBox(rows []colRow, cur, top int) []string {
	inner := 58
	if m := p.sc.cols - 6; m > 24 && inner > m {
		inner = m
	}
	labelW := 4
	for _, r := range rows {
		if w := vwidth(r.label); w > labelW {
			labelW = w
		}
	}
	if labelW > 12 {
		labelW = 12
	}
	bar := strings.Repeat("─", inner+2)
	line := func(s string) string { return dim("│") + " " + padCells(s, inner) + " " + dim("│") }
	box := []string{dim("╭" + bar + "╮"), line(ansi("1;36", "columns")), dim("├" + bar + "┤")}
	end := top + p.colBodyRows()
	if end > len(rows) {
		end = len(rows)
	}
	for i := top; i < end; i++ {
		box = append(box, line(p.colRowText(rows[i], i == cur, inner, labelW)))
	}
	box = append(box,
		dim("├"+bar+"┤"),
		line(ansi("2", "space show/hide · [ ] reorder · +/- width · r reset")),
		line(ansi("2", "enter save · esc cancel")),
		dim("╰"+bar+"╯"))
	return box
}

// colBodyRows is how many column rows the modal shows before scrolling, matching
// the clamp in columnEditor.
func (p *picker) colBodyRows() int {
	if h := p.sc.rows - 8; h > 3 {
		return h
	}
	return 3
}

// colRowText formats one column line: a visibility checkbox, the (clipped) label,
// its width, and a dimmed description. The cursor row is reverse-highlighted.
func (p *picker) colRowText(r colRow, sel bool, inner, labelW int) string {
	box := "[ ]"
	if r.visible {
		box = "[x]"
	}
	label := padCells(view.Clip(r.label, labelW), labelW)
	head := fmt.Sprintf("%s %s  w:%2d  ", box, label, r.width)
	room := inner - vwidth(head)
	help := ""
	if room > 1 {
		help = view.Clip(r.help, room)
	}
	if sel {
		return "\x1b[7m" + padCells(head+help, inner) + "\x1b[0m"
	}
	if !r.visible {
		return ansi("2", head+help) // a hidden column recedes as a whole
	}
	return head + ansi("2", help)
}
