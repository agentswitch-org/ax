package view

import (
	"testing"
	"time"
)

// Turn markers carry an absolute local YYYY-MM-DD HH:MM stamp: sessions span
// days, so a bare clock time would be ambiguous on the transcript.
func TestTurnTimeIsAbsoluteDateTime(t *testing.T) {
	ts := time.Date(2026, 7, 4, 9, 5, 0, 0, time.Local)
	if got, want := TurnTime(ts), "2026-07-04 09:05"; got != want {
		t.Fatalf("TurnTime = %q, want %q", got, want)
	}
	if got := TurnTime(time.Time{}); got != "?" {
		t.Fatalf("TurnTime(zero) = %q, want %q", got, "?")
	}
}
