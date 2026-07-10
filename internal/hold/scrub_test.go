package hold

import (
	"bytes"
	"testing"
)

// The attach client scrubs DEC private mode ENABLE sequences from replayed
// output so a held ConPTY's own mode-sets (win32-input-mode 9001, focus
// reporting 1004, mouse, bracketed paste) cannot arm the real terminal.
// Disables and every other escape must pass through untouched.
func TestModeScrubberStripsEnablesKeepsRest(t *testing.T) {
	in := []byte("\x1b[2J\x1b[Hhello \x1b[?1004h\x1b[31mred\x1b[0m \x1b[?9001hworld\x1b[?1000h\x1b[?2004h!")
	want := []byte("\x1b[2J\x1b[Hhello \x1b[31mred\x1b[0m world!")
	s := &modeScrubber{}
	if got := s.scrub(in); !bytes.Equal(got, want) {
		t.Errorf("scrub = %q, want %q", got, want)
	}
	if len(s.carry) != 0 {
		t.Errorf("carry = %q, want empty", s.carry)
	}
}

func TestModeScrubberKeepsDisables(t *testing.T) {
	in := []byte("a\x1b[?1004lb\x1b[?9001lc\x1b[?2004ld")
	s := &modeScrubber{}
	if got := s.scrub(in); !bytes.Equal(got, in) {
		t.Errorf("scrub = %q, want %q unchanged", got, in)
	}
}

// Keyboard-protocol enables must be stripped too: xterm modifyOtherKeys
// (CSI > 4;1m / 4;2m) and kitty keyboard protocol pushes and sets, whose
// flags vary. Replayed into a terminal that honors them (tmux with
// extended-keys on) they change what bytes Ctrl-A produces, which kills the
// detach chord. The matching disables (level 0, pops) pass through.
func TestModeScrubberStripsKeyboardProtocolEnables(t *testing.T) {
	in := []byte("a\x1b[>4;2mb\x1b[>7uc\x1b[>1ud\x1b[=5;1ue\x1b[>4;1mf")
	want := []byte("abcdef")
	s := &modeScrubber{}
	if got := s.scrub(in); !bytes.Equal(got, want) {
		t.Errorf("scrub = %q, want %q", got, want)
	}

	keep := []byte("a\x1b[<ub\x1b[>4;0mc\x1b[>4md\x1b[=ce")
	s = &modeScrubber{}
	if got := s.scrub(keep); !bytes.Equal(got, keep) {
		t.Errorf("scrub = %q, want %q unchanged", got, keep)
	}
}

// A kitty push split across two frames must still be stripped, and a held-back
// prefix that turns out to be a pop is emitted intact.
func TestModeScrubberKittySplitAcrossChunks(t *testing.T) {
	s := &modeScrubber{}
	var out []byte
	out = append(out, s.scrub([]byte("x\x1b[>"))...)
	out = append(out, s.scrub([]byte("7uy"))...)
	if want := []byte("xy"); !bytes.Equal(out, want) {
		t.Errorf("scrubbed chunks = %q, want %q", out, want)
	}

	s = &modeScrubber{}
	out = out[:0]
	out = append(out, s.scrub([]byte("x\x1b["))...)
	out = append(out, s.scrub([]byte("<uy"))...)
	if want := []byte("x\x1b[<uy"); !bytes.Equal(out, want) {
		t.Errorf("scrubbed chunks = %q, want %q", out, want)
	}
}

// A target sequence split across two frames must still be stripped: the
// scrubber holds a chunk-ending prefix back and re-scans it with the next
// chunk.
func TestModeScrubberSplitAcrossChunks(t *testing.T) {
	s := &modeScrubber{}
	var out []byte
	out = append(out, s.scrub([]byte("foo\x1b[?10"))...)
	out = append(out, s.scrub([]byte("04hbar"))...)
	if want := []byte("foobar"); !bytes.Equal(out, want) {
		t.Errorf("scrubbed chunks = %q, want %q", out, want)
	}

	// A held-back prefix that turns out to be a disable is emitted intact.
	s = &modeScrubber{}
	out = out[:0]
	out = append(out, s.scrub([]byte("x\x1b[?1004"))...)
	out = append(out, s.scrub([]byte("ly"))...)
	if want := []byte("x\x1b[?1004ly"); !bytes.Equal(out, want) {
		t.Errorf("scrubbed chunks = %q, want %q", out, want)
	}
}

func TestTerminalModeScrubberStripsSplitWin32InputEnable(t *testing.T) {
	s := &TerminalModeScrubber{}
	var out []byte
	out = append(out, s.Scrub([]byte("a\x1b[?90"))...)
	out = append(out, s.Scrub([]byte("01hb"))...)
	if want := []byte("ab"); !bytes.Equal(out, want) {
		t.Errorf("TerminalModeScrubber chunks = %q, want %q", out, want)
	}
}

func TestTerminalModeScrubberBuffersSynchronizedOutput(t *testing.T) {
	s := &TerminalModeScrubber{}
	var out []byte
	out = append(out, s.Scrub([]byte("before \x1b[?2026hpartial"))...)
	if want := []byte("before "); !bytes.Equal(out, want) {
		t.Fatalf("first chunk = %q, want %q", out, want)
	}
	out = append(out, s.Scrub([]byte(" frame\x1b[?2026l after"))...)
	want := []byte("before \x1b[?2026hpartial frame\x1b[?2026l after")
	if !bytes.Equal(out, want) {
		t.Errorf("buffered sync output = %q, want %q", out, want)
	}
}

func TestTerminalModeScrubberBuffersSplitSynchronizedMarkers(t *testing.T) {
	s := &TerminalModeScrubber{}
	var out []byte
	out = append(out, s.Scrub([]byte("x\x1b[?20"))...)
	out = append(out, s.Scrub([]byte("26hbody\x1b[?202"))...)
	out = append(out, s.Scrub([]byte("6ly"))...)
	want := []byte("x\x1b[?2026hbody\x1b[?2026ly")
	if !bytes.Equal(out, want) {
		t.Errorf("split synchronized output = %q, want %q", out, want)
	}
}

func TestTerminalModeScrubberFlushReleasesIncompleteSynchronizedOutput(t *testing.T) {
	s := &TerminalModeScrubber{}
	if got := s.Scrub([]byte("\x1b[?2026hbody")); len(got) != 0 {
		t.Fatalf("incomplete synchronized output emitted %q", got)
	}
	if got := s.Flush(); !bytes.Equal(got, []byte("\x1b[?2026hbody")) {
		t.Errorf("Flush = %q, want incomplete frame", got)
	}
}
