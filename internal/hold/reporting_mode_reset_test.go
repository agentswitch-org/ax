package hold

import "testing"

// ReportingModeReset is shared by the hold client's detach teardown and the
// run wrapper's (run_unix.go, run_windows.go) exit teardown, so both stop a
// harness's mouse/focus/paste reporting modes from leaking onto the shell
// prompt. Pin the exact escape sequence so a change to one path can't drift
// from the other.
func TestReportingModeReset(t *testing.T) {
	want := "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?2004l\x1b[?9001l\x1b[>4;0m\x1b[<u"
	if ReportingModeReset != want {
		t.Errorf("ReportingModeReset = %q, want %q", ReportingModeReset, want)
	}
}
