package view

import (
	_ "embed"
	"strings"

	"github.com/mattn/go-runewidth"

	"github.com/agentswitch-org/ax/internal/build"
	"github.com/agentswitch-org/ax/internal/keys"
)

//go:embed banner.txt
var bannerArt string

// help layout constants: the fixed spacing inside a panel. A key row is
// indent + keyW + gap + desc, where keyW and desc are sized at render time
// (keyW from the widest key label, desc from the terminal width). Keeping
// them here means both panels share the same key column, so every key and
// every description starts at the same screen column.
const (
	helpIndent  = 4 // leading spaces before a key
	helpGap     = 2 // spaces between the key column and the description
	helpSecPad  = 2 // section-title indent
	helpMinDesc = 8 // smallest description width before we stop shrinking
)

// hItem is one line inside a panel: a section header, a key row, or a blank
// spacer. Key rows keep the raw key label and description so the column widths
// can be measured across every row before anything is padded or truncated.
type hItem struct {
	kind      int // 0 blank, 1 section, 2 key row
	title     string
	key, desc string
}

// Help renders the boxed, two-column key table under the AgentSwitch banner,
// centered in a rows x cols screen. Rows come from the live keymap km, so the
// help always shows the keys actually in effect; detachKey is the in-session
// detach keystroke's label (config-driven, so it is passed in like the keymap).
//
// The overlay is responsive: the description column grows to fit the longest
// description up to a natural cap, then shrinks to fit a narrow terminal,
// truncating descriptions with an ellipsis rather than overflowing the frame.
// The banner is dropped when the screen is too short or too narrow to hold it.
func Help(km keys.Map, detachKey string, rows, cols int) string {
	cBord := func(s string) string { return ansi("90", s) }  // dim border
	cBan := func(s string) string { return ansi("1;36", s) } // banner, bold cyan
	cSec := func(s string) string { return ansi("1;33", s) } // section, bold yellow
	cKey := func(s string) string { return ansi("1;37", s) } // key, bold white
	cDim := func(s string) string { return ansi("2", s) }    // subtitle / footer

	vis := dispWidth

	blank := hItem{}
	section := func(name string) hItem { return hItem{kind: 1, title: name} }
	keyrow := func(k, d string) hItem { return hItem{kind: 2, key: k, desc: d} }

	addGroup := func(dst []hItem, title, group string) []hItem {
		dst = append(dst, section(title))
		for _, d := range keys.Defs {
			if d.Group == group {
				dst = append(dst, keyrow(strings.Join(km.Keys(d.Action), " / "), d.Desc))
			}
		}
		return dst
	}

	var left []hItem
	left = addGroup(left, "MOVE", "move")
	left = append(left, blank)
	left = addGroup(left, "SORT", "sort")
	left = append(left, blank)
	left = addGroup(left, "VIEW", "view")
	left = append(left, blank)
	left = addGroup(left, "SEARCH", "search")
	left = append(left, keyrow("esc", "leave the current mode, back to normal"))

	var right []hItem
	right = addGroup(right, "PREVIEW", "preview")
	right = append(right, blank, section("SESSION"), keyrow("enter", "open or resume the selection, no flags"))
	for _, d := range keys.Defs {
		if d.Group == "session" {
			if len(km.Keys(d.Action)) == 0 {
				continue // an action with no resolved key (e.g. New/NewArgs) has nothing to show
			}
			right = append(right, keyrow(strings.Join(km.Keys(d.Action), " / "), d.Desc))
		}
	}
	right = append(right, keyrow(strings.ToLower(detachKey), "detach an attached session, leaving it running"))
	right = append(right, blank)
	right = addGroup(right, "QUIT", "quit")

	// keyW: widest key label across both panels, so the key columns line up.
	// descIdeal: widest description, the natural (uncramped) size of the panel.
	keyW, descIdeal := 0, 0
	for _, list := range [][]hItem{left, right} {
		for _, it := range list {
			if it.kind == 2 {
				keyW = max(keyW, vis(it.key))
				descIdeal = max(descIdeal, vis(it.desc))
			}
		}
	}

	// colW is the content width of one panel. Start at the ideal (fits every
	// description), then clamp to the terminal so the outer box (2*colW+7)
	// fits in cols, with a floor that keeps a few description columns alive.
	colW := helpIndent + keyW + helpGap + descIdeal
	if fitCol := (cols - 7) / 2; fitCol < colW {
		colW = fitCol
	}
	if minCol := helpIndent + keyW + helpGap + helpMinDesc; colW < minCol {
		colW = minCol
	}
	descW := colW - helpIndent - keyW - helpGap
	W := 2*colW + 5 // inner width between the box borders
	sepPos := 1 + colW + 1

	// render a panel item to its colored string and its display width.
	renderItem := func(it hItem) (string, int) {
		switch it.kind {
		case 1:
			t := runewidth.Truncate(it.title, colW-helpSecPad, "…")
			return strings.Repeat(" ", helpSecPad) + cSec(t), helpSecPad + vis(t)
		case 2:
			d := it.desc
			if vis(d) > descW {
				d = runewidth.Truncate(d, descW, "…")
			}
			s := strings.Repeat(" ", helpIndent) + cKey(pad(it.key, keyW)) + strings.Repeat(" ", helpGap) + d
			return s, helpIndent + keyW + helpGap + vis(d)
		default:
			return "", 0
		}
	}

	border := func(l, fill, r string) string { return cBord(l + strings.Repeat(fill, W) + r) }
	junction := func(l, j, r string) string {
		runes := []rune(strings.Repeat("═", W))
		runes[sepPos] = []rune(j)[0]
		return cBord(l + string(runes) + r)
	}
	full := func(colored string, width int) string { // full-width centered row
		p := max(0, W-width)
		return cBord("║") + strings.Repeat(" ", p/2) + colored + strings.Repeat(" ", p-p/2) + cBord("║")
	}

	var b strings.Builder
	out := func(s string) { b.WriteString(s + "\n") }

	bannerLines := strings.Split(strings.TrimRight(bannerArt, "\n"), "\n")
	bannerW := 0
	for _, l := range bannerLines {
		bannerW = max(bannerW, vis(l))
	}
	n := max(len(left), len(right))
	// drop the banner when the box is too short (rows) or too narrow (W) to
	// hold it centered without overflowing the frame.
	withBanner := rows >= len(bannerLines)+n+8 && W >= bannerW

	out(border("╔", "═", "╗"))
	if withBanner {
		for _, line := range bannerLines {
			out(full(cBan(line), vis(line)))
		}
		sub := "agentswitch  ·  v" + build.Version
		out(full(cDim(sub), vis(sub)))
		out(junction("╠", "╤", "╣"))
	}
	for i := 0; i < n; i++ {
		ls, lw, rs, rw := "", 0, "", 0
		if i < len(left) {
			ls, lw = renderItem(left[i])
		}
		if i < len(right) {
			rs, rw = renderItem(right[i])
		}
		out(cBord("║") + " " + ls + strings.Repeat(" ", max(0, colW-lw)) +
			" " + cBord("│") + " " + rs + strings.Repeat(" ", max(0, colW-rw)) + " " + cBord("║"))
	}
	out(junction("╠", "╧", "╣"))
	foot := "feedback  ·  support@agentswitch.org"
	out(full(cDim(foot), vis(foot)))
	out(border("╚", "═", "╝"))

	// center the whole box on screen
	boxW := W + 2
	indent := strings.Repeat(" ", max(0, (cols-boxW)/2))
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	top := max(0, (rows-(len(lines)+2))/2)

	var screen strings.Builder
	screen.WriteString(strings.Repeat("\n", top))
	for _, l := range lines {
		screen.WriteString(indent + l + "\n")
	}
	tip := "press any key to close"
	screen.WriteString("\n" + strings.Repeat(" ", max(0, (cols-len(tip))/2)) + cDim(tip) + "\n")
	return screen.String()
}
