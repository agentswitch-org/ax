package finder

import (
	"strconv"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/view"
)

// netpanel.go is the picker's network/health overlay (default key `S`): a modal
// that lists every configured host, one row each, with its reachability +
// latency, ax/wire version (with a compat marker against local), OS/shell,
// harness set, and profile in-sync/drift verdict. It is the visual twin of `ax
// config status` and drives the same code paths for mutation: sync-to-host,
// sync-to-all, and rollback, each gated behind an in-overlay yes/no confirm
// (they overwrite a remote config). The status fan-out is slow (ssh), so the
// panel opens immediately in a "querying…" state and fills each row as its host
// answers off the UI goroutine; an offline / no-ax host lands late as that state
// and never hangs the panel. Mutations run off the UI goroutine too, so a slow
// push keeps the spinner alive rather than freezing the frame. It is a
// front-end: gather and every mutation are injected callbacks that reuse the
// app's status/sync/rollback code paths.

// netRowResult carries one host's completed status fetch back to the panel loop.
type netRowResult struct {
	name string
	row  view.NetHostStatus
}

// netActionResult is a finished mutation (sync/rollback): the status line to show
// and the hosts to re-query so their drift verdict refreshes.
type netActionResult struct {
	status  string
	refresh []string
}

// netPanelState is the overlay's working state: the configured host order, each
// host's fetched row (absent while still querying), which hosts are in flight,
// the cursor + scroll, and a transient status/result line.
type netPanelState struct {
	names    []string
	rows     map[string]view.NetHostStatus
	querying map[string]bool
	cursor   int
	top      int
	status   string
}

// current is the host name under the cursor, false when the roster is empty.
func (np *netPanelState) current() (string, bool) {
	if np.cursor < 0 || np.cursor >= len(np.names) {
		return "", false
	}
	return np.names[np.cursor], true
}

// move steps the cursor, clamped to the roster.
func (np *netPanelState) move(d int) {
	if len(np.names) == 0 {
		return
	}
	np.cursor = clamp(np.cursor+d, 0, len(np.names)-1)
}

// askConfirm gates a mutating action. It uses the injected confirmFn when set (a
// test seam) and otherwise the picker's own yes/no modal, so a sync or rollback
// never fires without an explicit yes.
func (p *picker) askConfirm(msg string) bool {
	if p.confirmFn != nil {
		return p.confirmFn(msg)
	}
	return p.confirm(msg)
}

// queryNetHost (re)fetches one host's status off the UI goroutine; the result
// lands on ch. The row is marked querying and its old row cleared so the panel
// shows the in-flight state at once. No-op without the gather callback.
func (p *picker) queryNetHost(np *netPanelState, ch chan netRowResult, name string) {
	if p.netStatusHost == nil {
		return
	}
	np.querying[name] = true
	delete(np.rows, name)
	go func() { ch <- netRowResult{name, p.netStatusHost(name)} }()
}

// netPanel runs the network/health overlay, blocking the picker while open (like
// the column and label modals). It opens immediately and streams rows in as each
// host answers, so the slow ssh fan-out never gates the first frame.
func (p *picker) netPanel() {
	if p.netHosts == nil {
		p.notice = "network panel needs configured [[host]] entries"
		return
	}
	names := p.netHosts()
	if len(names) == 0 {
		p.notice = "no [[host]] configured for the network panel"
		return
	}
	np := &netPanelState{
		names:    names,
		rows:     map[string]view.NetHostStatus{},
		querying: map[string]bool{},
		status:   "querying " + strconv.Itoa(len(names)) + " host" + plural(len(names)) + "…",
	}
	ch := make(chan netRowResult, len(names)+8)   // buffered so a late reply never blocks a closed panel
	act := make(chan netActionResult, len(names)) // completed mutations report back here
	for _, n := range names {
		p.queryNetHost(np, ch, n)
	}
	tk := time.NewTicker(120 * time.Millisecond) // spinner + late-fill repaint
	defer tk.Stop()
	for {
		p.sc.render(p.overlay(p.netPanelBox(np)))
		select {
		case r := <-ch:
			delete(np.querying, r.name)
			np.rows[r.name] = r.row
			if len(np.querying) == 0 && strings.HasSuffix(np.status, "…") {
				np.status = p.netControlsHint()
			}
		case ar := <-act:
			np.status = ar.status
			for _, n := range ar.refresh {
				p.queryNetHost(np, ch, n)
			}
		case <-tk.C:
			p.frame++
		case e := <-p.sc.events:
			switch e.t {
			case evEsc, evCtrlC:
				p.previewDirty = true
				return
			case evUp:
				np.move(-1)
			case evDown:
				np.move(1)
			case evRune:
				switch e.r {
				case 'q':
					p.previewDirty = true
					return
				case 'k':
					np.move(-1)
				case 'j':
					np.move(1)
				case 'r':
					np.status = "refreshing…"
					for _, n := range np.names {
						p.queryNetHost(np, ch, n)
					}
				case 's':
					p.netSyncSelected(np, act)
				case 'a':
					p.netSyncAllHosts(np, act)
				case 'b':
					p.netRollbackSelected(np, act)
				}
			}
		}
	}
}

// netSyncSelected confirms, then pushes the local profile to the host under the
// cursor (the `ax config sync --host` path) OFF the UI goroutine, reporting the
// result on act. The confirm is mandatory: sync overwrites the remote config.
func (p *picker) netSyncSelected(np *netPanelState, act chan netActionResult) {
	name, ok := np.current()
	if !ok || p.netSync == nil {
		return
	}
	if !p.askConfirm("sync profile to " + name + "?  overwrites its remote config") {
		return
	}
	np.status = "syncing " + name + "…"
	go func() { act <- netActionResult{"sync " + name + ": " + p.netSync(name), []string{name}} }()
}

// netSyncAllHosts confirms, then pushes the local profile to every configured
// host (the `ax config sync --all` path) off the UI goroutine.
func (p *picker) netSyncAllHosts(np *netPanelState, act chan netActionResult) {
	if p.netSyncAll == nil {
		return
	}
	if !p.askConfirm("sync profile to ALL " + strconv.Itoa(len(np.names)) + " hosts?  overwrites each remote config") {
		return
	}
	np.status = "syncing all…"
	names := append([]string(nil), np.names...)
	go func() { act <- netActionResult{"sync all: " + p.netSyncAll(), names} }()
}

// netRollbackSelected confirms, then rolls the host under the cursor back to its
// latest config backup (the `ax config rollback --host` path) off the UI goroutine.
func (p *picker) netRollbackSelected(np *netPanelState, act chan netActionResult) {
	name, ok := np.current()
	if !ok || p.netRollback == nil {
		return
	}
	if !p.askConfirm("roll back " + name + " to its latest config backup?") {
		return
	}
	np.status = "rolling back " + name + "…"
	go func() { act <- netActionResult{"rollback " + name + ": " + p.netRollback(name), []string{name}} }()
}

// netControlsHint is the footer control line, also the resting status once every
// host has answered.
func (p *picker) netControlsHint() string {
	return "j/k move · r refresh · s sync · a sync all · b rollback · esc close"
}

// netPanelBodyRows is how many host rows the overlay shows before it scrolls,
// sized to the screen with a small floor. Headless (tests) gets the whole roster.
func (p *picker) netPanelBodyRows(np *netPanelState) int {
	if p.sc == nil {
		return len(np.names)
	}
	if h := p.sc.rows - 8; h > 3 {
		return h
	}
	return 3
}

// netCell is one column of a host row: its (pre-color) text, an optional SGR
// code, and a fixed pad width (0 = a flexible column that takes the rest).
type netCell struct {
	text string
	code string
	pad  int
}

// netPanelBox renders the overlay: a titled box with one row per host (scrolled
// to the cursor), a status/result line, and the control footer. Responsive like
// the help and column overlays: the inner width tracks the terminal and every
// row is clipped with an ellipsis rather than overflowing the frame.
func (p *picker) netPanelBox(np *netPanelState) []string {
	inner := 72
	if p.sc != nil {
		// Always shrink to the terminal (down to a small floor) so the box never
		// overflows the frame; the rows truncate with an ellipsis inside it.
		if m := p.sc.cols - 6; m < inner && m > 8 {
			inner = m
		}
	}
	nameW := 4
	for _, n := range np.names {
		if w := vwidth(n); w > nameW {
			nameW = w
		}
	}
	if nameW > 16 {
		nameW = 16
	}

	body := p.netPanelBodyRows(np)
	// clamp the scroll window to the cursor before slicing.
	if np.cursor < np.top {
		np.top = np.cursor
	}
	if np.cursor >= np.top+body {
		np.top = np.cursor - body + 1
	}
	if np.top < 0 {
		np.top = 0
	}
	end := np.top + body
	if end > len(np.names) {
		end = len(np.names)
	}

	bar := strings.Repeat("─", inner+2)
	line := func(s string) string { return dim("│") + " " + padCells(s, inner) + " " + dim("│") }
	title := "network · " + strconv.Itoa(len(np.names)) + " host" + plural(len(np.names))
	box := []string{dim("╭" + bar + "╮"), line(ansi("1;36", title)), dim("├" + bar + "┤")}
	for i := np.top; i < end; i++ {
		box = append(box, line(p.netRowText(np, np.names[i], i == np.cursor, inner, nameW)))
	}
	status := np.status
	if status == "" {
		status = p.netControlsHint()
	}
	box = append(box,
		dim("├"+bar+"┤"),
		line(ansi("2", view.Clip(status, inner))),
		line(ansi("2", view.Clip(p.netControlsHint(), inner))),
		dim("╰"+bar+"╯"))
	return box
}

// netRowText formats one host row into cells and lays them out left to right,
// clipping to the inner width so a narrow terminal truncates rather than
// overflows. The cursor row is reverse-highlighted.
func (p *picker) netRowText(np *netPanelState, name string, sel bool, inner, nameW int) string {
	cells := p.netRowCells(np, name, nameW)
	var b strings.Builder
	used := 0
	for i, c := range cells {
		if i > 0 {
			if used+2 > inner {
				break
			}
			b.WriteString("  ")
			used += 2
		}
		room := inner - used
		if room <= 0 {
			break
		}
		target := c.pad
		if target == 0 || target > room {
			target = room // flexible column, or a fixed one that no longer fits: take what's left
		}
		seg := c.text
		if vwidth(seg) > target {
			seg = view.Clip(seg, target)
		} else if c.pad > 0 {
			seg = padCells(seg, target)
		}
		used += vwidth(seg)
		if c.code != "" {
			b.WriteString(ansi(c.code, seg))
		} else {
			b.WriteString(seg)
		}
	}
	out := b.String()
	if sel {
		return "\x1b[7m" + padCells(view.StripANSI(out), inner) + "\x1b[0m"
	}
	return out
}

// netRowCells builds a host row's columns: name, reachability + latency, profile
// sync, ax/wire version, and a flexible OS/shell/harness detail. A host still
// being queried shows a spinner in place of its state; sync (the reason the panel
// exists) is column two so it survives a narrow width.
func (p *picker) netRowCells(np *netPanelState, name string, nameW int) []netCell {
	cells := []netCell{{text: name, pad: nameW}}
	if np.querying[name] {
		sp := loadSpinner[p.frame%len(loadSpinner)]
		return append(cells,
			netCell{text: sp + " querying", code: "2", pad: 12},
			netCell{text: "…", code: "2", pad: 8})
	}
	r, ok := np.rows[name]
	if !ok {
		return append(cells, netCell{text: "…", code: "2", pad: 12})
	}
	stText, stCode := netStateCell(r)
	syText, syCode := netSyncCell(r.Sync)
	return append(cells,
		netCell{text: stText, code: stCode, pad: 12},
		netCell{text: syText, code: syCode, pad: 8},
		netCell{text: netVersionCell(r), pad: 18},
		netCell{text: netDetailCell(r), code: "2", pad: 0})
}

// netStateCell renders reachability: an online host shows "ok" + latency, an
// offline / no-ax host shows why, colored by severity.
func netStateCell(r view.NetHostStatus) (string, string) {
	switch r.State {
	case view.HostOnline:
		if r.LatencyMS > 0 {
			return "ok " + strconv.FormatInt(r.LatencyMS, 10) + "ms", "1;32"
		}
		return "ok", "1;32"
	case view.HostNoAx:
		return "no ax", "1;33"
	case view.HostOffline:
		return "offline", "1;31"
	}
	if r.State == "" {
		return "unknown", "2"
	}
	return r.State, "2"
}

// netSyncCell colors the profile drift verdict: in-sync green, drift yellow,
// unreachable/unknown dim.
func netSyncCell(sync string) (string, string) {
	switch sync {
	case "in-sync":
		return "in-sync", "1;32"
	case "drift":
		return "drift", "1;33"
	case "unreachable":
		return "unreach", "1;31"
	}
	if sync == "" {
		return "unknown", "2"
	}
	return sync, "2"
}

// netVersionCell is the ax version with its wire-compat marker, or "-" when the
// host did not report one (offline, or a pre-capability ax).
func netVersionCell(r view.NetHostStatus) string {
	if !r.Reachable {
		return "-"
	}
	v := r.AxVersion
	if v == "" {
		v = "unknown"
	}
	return v + " (v" + strconv.Itoa(r.WireVersion) + " " + r.Compat + ")"
}

// netDetailCell folds OS/shell and the harness set into one dimmed, flexible
// column; empty (rendered as "-") when the host reported none.
func netDetailCell(r view.NetHostStatus) string {
	var parts []string
	if r.OS != "" {
		parts = append(parts, r.OS)
	}
	if r.Shell != "" {
		parts = append(parts, r.Shell)
	}
	det := strings.Join(parts, " ")
	if len(r.Harnesses) > 0 {
		if det != "" {
			det += " · "
		}
		det += strings.Join(r.Harnesses, ",")
	}
	if det == "" {
		return "-"
	}
	return det
}
