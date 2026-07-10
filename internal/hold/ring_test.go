package hold

import (
	"bytes"
	"strings"
	"testing"
)

// Under capacity the ring is a plain transcript: everything written comes back
// verbatim (no boundary trimming, nothing was dropped).
func TestRingUnderCapacity(t *testing.T) {
	r := NewRing(64)
	r.Write([]byte("hello "))
	r.Write([]byte("world"))
	if got := string(r.Snapshot()); got != "hello world" {
		t.Errorf("snapshot = %q, want %q", got, "hello world")
	}
}

// Over capacity only the newest bytes survive, whether the overflow comes from
// many small writes or one write larger than the whole ring.
func TestRingKeepsNewest(t *testing.T) {
	r := NewRing(10)
	r.Write([]byte("aaaaa"))
	r.Write([]byte("bbbbb"))
	r.Write([]byte("ccccc")) // pushes the a's (and the trim point) out
	snap := string(r.Snapshot())
	if !strings.HasSuffix(snap, "ccccc") || len(snap) > 10 {
		t.Errorf("snapshot = %q, want the newest <= 10 bytes ending in ccccc", snap)
	}
	if strings.Contains(snap, "a") {
		t.Errorf("snapshot = %q still holds evicted bytes", snap)
	}

	r2 := NewRing(8)
	r2.Write(bytes.Repeat([]byte("x"), 100))
	if got := len(r2.Snapshot()); got > 8 {
		t.Errorf("oversized single write kept %d bytes, cap is 8", got)
	}
}

// A wrapped ring's front may cut an escape sequence in half; the snapshot must
// skip forward to the next fresh start (the next ESC) so the replay never
// re-drives the terminal with half a sequence.
func TestRingTrimsToEscapeBoundary(t *testing.T) {
	r := NewRing(16)
	r.Write([]byte("aaaaaaaaaaaaaaaa"))  // fill
	r.Write([]byte("39;49m\x1b[1mBOLD")) // overflow: front now starts mid-sequence ("9;49m...")
	snap := r.Snapshot()
	if !bytes.HasPrefix(snap, []byte("\x1b[1mBOLD")) {
		t.Errorf("snapshot = %q, want it to start at the next ESC", snap)
	}
}

// With no ESC ahead, the trim falls to just past the next newline, the other
// safe restart point for plain line output.
func TestRingTrimsPastNewline(t *testing.T) {
	r := NewRing(16)
	r.Write([]byte("aaaaaaaaaaaaaaaa"))
	r.Write([]byte("partial\nline two")) // wraps; no ESC anywhere
	if got := string(r.Snapshot()); got != "line two" {
		t.Errorf("snapshot = %q, want %q", got, "line two")
	}
}

// When the ring holds an alternate-screen enter, the replay starts at the most
// recent one: a full-screen app then repaints from its own clean switch, and
// everything before the switch (stale main-screen scrollback) is dropped.
func TestRingAltScreenTrim(t *testing.T) {
	r := NewRing(256)
	r.Write([]byte("old scrollback\n"))
	r.Write([]byte("\x1b[?1049hfirst screen"))
	r.Write([]byte("\x1b[?1049hSECOND screen"))
	if got := string(r.Snapshot()); got != "\x1b[?1049hSECOND screen" {
		t.Errorf("snapshot = %q, want the replay to start at the last alt-screen enter", got)
	}

	// The 47h legacy form counts too.
	r2 := NewRing(256)
	r2.Write([]byte("noise\x1b[?47hAPP"))
	if got := string(r2.Snapshot()); got != "\x1b[?47hAPP" {
		t.Errorf("snapshot = %q, want %q", got, "\x1b[?47hAPP")
	}
}

// Some terminal apps redraw the main screen from home instead of entering the
// alternate screen. Reattach should show the newest frame, not every incremental
// redraw that led up to it.
func TestRingHomeRedrawTrim(t *testing.T) {
	r := NewRing(256)
	r.Write([]byte("old frame\n\x1b[Hfeature you"))
	r.Write([]byte("\x1b[Hfeature you want to add, or a completely different project"))
	if got := string(r.Snapshot()); got != "\x1b[Hfeature you want to add, or a completely different project" {
		t.Errorf("snapshot = %q, want latest home redraw", got)
	}
}

func TestTrimReplayPayloadUsesLatestHomeRedraw(t *testing.T) {
	in := []byte("old frame\n\x1b[Hpartial\x1b[Hfinal")
	if got := string(TrimReplayPayload(in)); got != "\x1b[Hfinal" {
		t.Errorf("TrimReplayPayload = %q, want latest home redraw", got)
	}
}

// Alternate-screen entry has to win over later cursor-home frames: the viewer
// needs the mode enter before replaying any frame painted inside that screen.
func TestRingAltScreenBeatsHomeRedraw(t *testing.T) {
	r := NewRing(256)
	r.Write([]byte("old scrollback\n\x1b[?1049h\x1b[Hfirst\x1b[Hsecond"))
	if got := string(r.Snapshot()); got != "\x1b[?1049h\x1b[Hfirst\x1b[Hsecond" {
		t.Errorf("snapshot = %q, want replay to include alt-screen enter", got)
	}
}

// An empty ring snapshots empty (a fresh attach before any output).
func TestRingEmpty(t *testing.T) {
	if got := NewRing(0).Snapshot(); len(got) != 0 {
		t.Errorf("empty ring snapshot = %q, want empty", got)
	}
}
