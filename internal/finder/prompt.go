package finder

import "strings"

// runPrompt reads a single line of text on its own screen (the new-subdir name
// and the per-launch args editor). It shares the process keyboard reader, so it
// never competes with the pickers for input. initial pre-fills the line (editable);
// Enter accepts; Esc returns empty.
func runPrompt(label, initial string) string {
	sc, err := openScreen()
	if err != nil {
		return ""
	}
	defer sc.close()

	text := initial
	for {
		w := sc.cols
		L := []string{
			fit(ansi("1;36", "  "+label), w),
			fit("  ❯ "+text+"\x1b[7m \x1b[0m", w),
			ansi("90", strings.Repeat("─", w)),
		}
		for len(L) < sc.rows {
			L = append(L, "")
		}
		sc.render(L)

		e := <-sc.events
		switch e.t {
		case evRune:
			text += string(e.r)
		case evBack:
			if text != "" {
				text = dropLastRune(text)
			}
		case evEnter:
			return text
		case evEsc, evCtrlC:
			return ""
		}
	}
}

// runPromptMultiline reads a multiline block of text on its own screen (the
// compose task prompt and inline-behavior editor). It shares the process
// keyboard reader like runPrompt, so it never competes with the pickers for
// input. Enter inserts a newline, Ctrl-D accepts, Esc/Ctrl-C cancels (ok=false).
// The trailing block cursor sits at the end of the last line; editing is
// append/backspace only (v1), which is enough to type and correct a prompt.
func runPromptMultiline(label, header, initial string) (string, bool) {
	sc, err := openScreen()
	if err != nil {
		return "", false
	}
	defer sc.close()

	text := initial
	for {
		w := sc.cols
		var L []string
		L = append(L, fit(ansi("1;36", "  "+label), w))
		if header != "" {
			L = append(L, fit(ansi("2", "  "+header), w))
		}
		L = append(L, fit(ansi("90", "  enter: newline   ctrl-d: accept   esc: cancel"), w))
		L = append(L, ansi("90", strings.Repeat("─", w)))
		lines := strings.Split(text, "\n")
		for i, ln := range lines {
			cursor := ""
			if i == len(lines)-1 {
				cursor = "\x1b[7m \x1b[0m" // the block cursor trails the last line
			}
			L = append(L, fit("  "+ln+cursor, w))
		}
		for len(L) < sc.rows {
			L = append(L, "")
		}
		sc.render(L)

		next, done, ok := multilineKey(text, <-sc.events)
		if done {
			if ok {
				return next, true
			}
			return "", false
		}
		text = next
	}
}

// multilineKey applies one decoded key to the multiline editor buffer. done
// reports the editor should close; ok then distinguishes accept (Ctrl-D) from
// cancel (Esc/Ctrl-C). While the editor stays open (done=false), next is the
// updated buffer: a rune appends, Backspace drops the last rune, Enter inserts a
// newline, and any other Ctrl chord is ignored. Pure, so the key handling is
// unit-testable without a terminal.
func multilineKey(text string, e ev) (next string, done, ok bool) {
	switch e.t {
	case evRune:
		return text + string(e.r), false, false
	case evBack:
		if text != "" {
			text = dropLastRune(text)
		}
		return text, false, false
	case evEnter:
		return text + "\n", false, false
	case evCtrl:
		if e.r == 'd' {
			return text, true, true
		}
		return text, false, false
	case evEsc, evCtrlC:
		return text, true, false
	}
	return text, false, false
}
