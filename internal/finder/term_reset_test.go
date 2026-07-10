package finder

import (
	"strings"
	"testing"
)

// The finder returns the terminal to the shell on close and suspend. Both write
// screenTeardown, which must clear every reporting mode a harness run in a prior
// session may have left on, so the shell prompt does not fill with leaked
// mouse/focus/paste reports.
func TestScreenTeardownDisablesReportingModes(t *testing.T) {
	for _, want := range []string{
		"\x1b[?1000l", // mouse click tracking
		"\x1b[?1002l", // mouse button-drag tracking
		"\x1b[?1003l", // mouse any-motion tracking
		"\x1b[?1006l", // SGR mouse encoding
		"\x1b[?1004l", // focus reporting
		"\x1b[?2004l", // bracketed paste
		"\x1b[?9001l", // Windows win32-input-mode
	} {
		if !strings.Contains(screenTeardown, want) {
			t.Errorf("screenTeardown missing reporting-mode disable %q", want)
		}
	}
	// The pre-existing restores must still be there: a clean local terminal has
	// to look identical on the happy path.
	for _, want := range []string{
		"\x1b[?7h",    // autowrap
		"\x1b[?25h",   // cursor
		"\x1b[?1049l", // leave alt screen
	} {
		if !strings.Contains(screenTeardown, want) {
			t.Errorf("screenTeardown missing existing restore %q", want)
		}
	}
}
