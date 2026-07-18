package finder

import (
	"encoding/base64"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	"github.com/agentswitch-org/ax/internal/fuzzy"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

// jumpRows is how many candidates the jump modal shows at once.
const jumpRows = 8

// jumpModal is the quick switcher (`'`): fuzzy-find ANY session by name,
// title, task, dir, or tag, ignoring the picker's scope, archive, and filter
// state entirely, and open it on Enter. It is the "I know its name, take me
// there" path; the table's own filters are for narrowing a view, not a jump.
func (p *picker) jumpModal() (session.Session, bool) {
	// Candidates once, most recent first: p.all is already Last-sorted on load,
	// but a column sort may have reordered it, so re-sort a copy by recency.
	cands := append([]session.Session(nil), p.all...)
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Last.After(cands[j].Last) })
	texts := make([]string, len(cands))
	for i, s := range cands {
		texts[i] = strings.Join([]string{
			s.Name, s.Title, s.Task, view.TildePath(s.Dir), strings.Join(s.Labels, " "), s.Harness, s.ID,
		}, " ")
	}
	query := ""
	cur := 0
	for {
		shown := jumpMatches(query, cands, texts)
		if cur >= len(shown) {
			cur = len(shown) - 1
		}
		if cur < 0 {
			cur = 0
		}
		p.sc.render(p.overlay(p.jumpBox(query, shown, cur)))
		e := <-p.sc.events
		switch e.t {
		case evEsc, evCtrlC:
			return session.Session{}, false
		case evEnter:
			if len(shown) > 0 {
				return shown[cur], true
			}
		case evUp:
			cur = max(cur-1, 0)
		case evDown:
			cur++
		case evBack:
			if query != "" {
				query = dropLastRune(query)
				cur = 0
			}
		case evRune:
			query += string(e.r)
			cur = 0
		}
	}
}

// jumpMatches ranks the candidates for the query; empty query = most recent.
func jumpMatches(query string, cands []session.Session, texts []string) []session.Session {
	if strings.TrimSpace(query) == "" {
		if len(cands) > jumpRows {
			return cands[:jumpRows]
		}
		return cands
	}
	var out []session.Session
	for _, i := range fuzzy.Rank(strings.TrimSpace(query), texts) {
		out = append(out, cands[i])
		if len(out) == jumpRows {
			break
		}
	}
	return out
}

// jumpBox renders the switcher: the query line, then the ranked candidates as
// "name · dir · age" with the harness hue on the name and the pick reversed.
func (p *picker) jumpBox(query string, shown []session.Session, cur int) []string {
	inner := 64
	if m := p.sc.cols - 6; m > 0 && inner > m {
		inner = m
	}
	bar := strings.Repeat("─", inner+2)
	row := func(s string) string { return dim("│") + " " + padCells(s, inner) + " " + dim("│") }
	box := []string{
		dim("╭" + bar + "╮"),
		row(ansi("1;36", "jump ❯ ") + query + "\x1b[7m \x1b[0m"),
		dim("├" + bar + "┤"),
	}
	if len(shown) == 0 {
		box = append(box, row(ansi("2", "(no match)")))
	}
	for i, s := range shown {
		name := s.Name
		if name == "" {
			name = s.Title
		}
		if name == "" {
			name = "(no title)"
		}
		line := view.Clip(name, inner*2/5) + ansi("2", " · "+view.Clip(view.TildePath(s.Dir), inner*2/5)+" · "+view.Age(s.Last))
		if i == cur {
			line = "\x1b[7m" + padCells(view.StripANSI(line), inner) + "\x1b[0m"
		}
		box = append(box, row(line))
	}
	box = append(box,
		dim("├"+bar+"┤"),
		row(ansi("2", "type to find · enter opens · esc closes")),
		dim("╰"+bar+"╯"))
	return box
}

// yankSel copies the focused session's id to the clipboard: a native clipboard
// tool when one exists, else OSC 52 straight to the terminal (which reaches
// the system clipboard through ssh and tmux on modern terminals).
func (p *picker) yankSel() {
	s, ok := p.cur()
	if !ok {
		return
	}
	if !yankNative(s.ID) {
		p.sc.out.WriteString("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s.ID)) + "\x07")
	}
	p.notice = ansi("1;36", "yanked "+s.ID)
}

// yankNative tries the platform clipboard tools; false means fall back to OSC 52.
func yankNative(text string) bool {
	var argv []string
	switch {
	case runtime.GOOS == "darwin":
		argv = []string{"pbcopy"}
	default:
		for _, c := range [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
			if _, err := exec.LookPath(c[0]); err == nil {
				argv = c
				break
			}
		}
	}
	if len(argv) == 0 {
		return false
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return false
	}
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin = strings.NewReader(text)
	return c.Run() == nil
}
