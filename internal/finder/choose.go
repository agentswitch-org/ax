package finder

import (
	"strings"

	"github.com/mattn/go-runewidth"

	"github.com/agentswitch-org/ax/internal/fuzzy"
)

// runChoose is a filterable single-select list (harness, directory, and relink
// pickers), rendered as a centered modal box sized to its content rather than a
// full-screen takeover. Ranking is in-process (see fuzzy). browse, when non-nil,
// supplies candidates fresh from the query on each edit (filesystem browsing).
// expect lists extra accept keys; the pressed one is returned (empty for Enter).
// item is "" on abort.
func runChoose(prompt, header string, items []string, browse func(string) []string, expect []string, backdrop []string) (string, string, error) {
	sc, err := openScreen()
	if err != nil {
		return "", "", err
	}
	defer sc.close()

	query := ""
	var matches []string
	cursor, top := 0, 0

	// browse (when set) regenerates the candidate set from the current query on
	// every edit, e.g. the subdirs of a typed path, so the list can follow you
	// into the filesystem instead of being a fixed set.
	refilter := func() {
		base := items
		if browse != nil {
			base = browse(query)
		}
		if strings.TrimSpace(query) == "" {
			matches = base
		} else {
			matches = nil
			for _, k := range fuzzy.Rank(query, base) {
				matches = append(matches, base[k])
			}
		}
		cursor = clamp(cursor, 0, max(len(matches)-1, 0))
	}
	refilter()

	wantTab := false
	for _, e := range expect {
		if e == "tab" {
			wantTab = true
		}
	}

	// vimNav enables j/k/g/G navigation on an empty query. It is on for plain
	// selection lists (harness, machine, relink) so vim keys move the cursor like
	// in the main picker; it stays off for the filesystem-browse dir picker, where
	// typing a path is the primary interaction. Arrow keys always navigate.
	vimNav := browse == nil

	for {
		base := backdrop
		if len(base) == 0 {
			base = make([]string, sc.rows)
		} else {
			base = append([]string(nil), base...) // composite mutates its base
		}
		sc.render(composite(base, chooseBox(sc, header, prompt, query, matches, cursor, &top), sc.rows, sc.cols))
		e := <-sc.events
		switch e.t {
		case evRune:
			if vimNav && query == "" {
				switch e.r {
				case 'j':
					if cursor < len(matches)-1 {
						cursor++
					}
					continue
				case 'k':
					if cursor > 0 {
						cursor--
					}
					continue
				case 'g':
					cursor = 0
					continue
				case 'G':
					cursor = max(len(matches)-1, 0)
					continue
				}
			}
			query += string(e.r)
			refilter()
		case evBack:
			if query != "" {
				query = dropLastRune(query)
				refilter()
			}
		case evUp:
			if cursor > 0 {
				cursor--
			}
		case evDown:
			if cursor < len(matches)-1 {
				cursor++
			}
		case evEsc, evCtrlC:
			return "", "", nil
		case evEnter:
			if cursor >= 0 && cursor < len(matches) {
				return matches[cursor], "", nil
			}
		case evTab:
			if wantTab && cursor >= 0 && cursor < len(matches) {
				return matches[cursor], "tab", nil
			}
		}
	}
}

// chooseBox renders the chooser as a centered, rounded box: an optional header,
// the query prompt, a divider, and the scrollable item list, sized to the widest
// content instead of the whole screen. It updates *top to keep the cursor visible.
func chooseBox(sc *screen, header, prompt, query string, matches []string, cursor int, top *int) []string {
	pr := prompt
	if pr == "" {
		pr = "❯ "
	}

	innerW := vwidth(pr) + vwidth(query) + 1 // +1 for the cursor block
	if header != "" && vwidth(header) > innerW {
		innerW = vwidth(header)
	}
	for _, m := range matches {
		if w := vwidth(m) + 2; w > innerW { // 2 for the row indent
			innerW = w
		}
	}
	if innerW < 24 {
		innerW = 24
	}
	if m := sc.cols - 6; m > 0 && innerW > m {
		innerW = m
	}

	listH := len(matches)
	if m := min(sc.rows-6, 12); listH > m {
		listH = m
	}
	if listH < 1 {
		listH = 1
	}
	if cursor < *top {
		*top = cursor
	}
	if cursor >= *top+listH {
		*top = cursor - listH + 1
	}
	if *top < 0 {
		*top = 0
	}

	bar := strings.Repeat("─", innerW+2)
	row := func(inner string) string { return dim("│") + " " + inner + " " + dim("│") }
	cell := func(s string) string { return padCells(runewidth.Truncate(s, innerW, ""), innerW) }

	var box []string
	box = append(box, dim("╭"+bar+"╮"))
	if header != "" {
		box = append(box, row(ansi("2", cell(header))))
	}
	box = append(box, row(padCells(ansi("1;36", pr)+query+"\x1b[7m \x1b[0m", innerW)))
	box = append(box, dim("├"+bar+"┤"))
	for r := 0; r < listH; r++ {
		mi := *top + r
		if mi >= len(matches) {
			box = append(box, row(strings.Repeat(" ", innerW)))
			continue
		}
		text := cell("  " + matches[mi])
		if mi == cursor {
			text = "\x1b[7m" + text + "\x1b[0m"
		}
		box = append(box, row(text))
	}
	box = append(box, dim("╰"+bar+"╯"))
	return box
}
