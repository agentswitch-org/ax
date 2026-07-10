package hold

import "bytes"

// scrubEnableSeqs are the exact ENABLE sequences ax strips from harness output
// before it reaches the real terminal: the modes ReportingModeReset disables on
// teardown and the xterm modifyOtherKeys levels (CSI > 4;1m / 4;2m). None of
// these may arm the outer terminal: reporting modes spray reports onto the shell
// prompt, and a keyboard-protocol enable replayed into a terminal that honors it
// changes what bytes Ctrl-A produces, which kills the detach chord. Only the
// enables are stripped; the matching disables pass through, so the harness can
// still turn a mode off if an enable slipped through. The kitty keyboard
// protocol enables carry variable flags, so they are matched by matchKittyEnable
// instead of this table.
var scrubEnableSeqs = func() [][]byte {
	seqs := make([][]byte, 0, len(reportingModes)+2)
	for _, c := range reportingModes {
		seqs = append(seqs, []byte("\x1b[?"+c+"h"))
	}
	seqs = append(seqs, []byte("\x1b[>4;1m"), []byte("\x1b[>4;2m"))
	return seqs
}()

// modeScrubber removes scrubEnableSeqs from a byte stream fed to it in
// arbitrary chunks. A sequence split across chunks still matches: a chunk
// ending in a proper prefix of a target sequence is held back (carry) and
// re-scanned with the next chunk. Everything else, including every other
// escape sequence, passes through byte-for-byte.
type modeScrubber struct {
	carry []byte
}

func (s *modeScrubber) scrub(p []byte) []byte {
	in := p
	if len(s.carry) > 0 {
		in = append(s.carry, p...)
		s.carry = nil
	}
	var out []byte
	for i := 0; i < len(in); {
		if in[i] != 0x1b {
			j := bytes.IndexByte(in[i:], 0x1b)
			if j < 0 {
				return append(out, in[i:]...)
			}
			out = append(out, in[i:i+j]...)
			i += j
		}
		n, partial := matchEnable(in[i:])
		switch {
		case n > 0:
			i += n
		case partial:
			s.carry = append(s.carry, in[i:]...)
			return out
		default:
			out = append(out, in[i])
			i++
		}
	}
	return out
}

// TerminalModeScrubber strips terminal mode ENABLE sequences from harness output
// before that output reaches the user's real terminal. It also emulates xterm
// synchronized output by buffering a frame from CSI ? 2026 h through CSI ? 2026 l
// and releasing it in one write. That keeps TUIs smooth through terminals or tmux
// paths that pass the markers through but do not implement the buffering.
type TerminalModeScrubber struct {
	inner modeScrubber
	sync  syncSmoother
}

func (s *TerminalModeScrubber) Scrub(p []byte) []byte {
	return s.sync.smooth(s.inner.scrub(p))
}

// Flush releases any incomplete synchronized-output frame. It is only for
// teardown/error paths; normal output should flush on the matching CSI ? 2026 l.
func (s *TerminalModeScrubber) Flush() []byte {
	return s.sync.flush()
}

const maxSyncBuffer = 4 << 20

var (
	syncStart = []byte("\x1b[?2026h")
	syncEnd   = []byte("\x1b[?2026l")
)

type syncSmoother struct {
	active bool
	buf    []byte
	carry  []byte
}

func (s *syncSmoother) smooth(p []byte) []byte {
	in := p
	if len(s.carry) > 0 {
		in = append(s.carry, p...)
		s.carry = nil
	}
	var out []byte
	for len(in) > 0 {
		if s.active {
			s.buf = append(s.buf, in...)
			in = nil
			if len(s.buf) > maxSyncBuffer {
				out = append(out, s.buf...)
				s.buf = nil
				s.active = false
				continue
			}
			if i := bytes.Index(s.buf, syncEnd); i >= 0 {
				end := i + len(syncEnd)
				out = append(out, s.buf[:end]...)
				rest := append([]byte(nil), s.buf[end:]...)
				s.buf = nil
				s.active = false
				in = rest
			}
			continue
		}
		i := bytes.IndexByte(in, 0x1b)
		if i < 0 {
			out = append(out, in...)
			break
		}
		out = append(out, in[:i]...)
		switch n, partial := matchLiteral(in[i:], syncStart); {
		case n > 0:
			s.active = true
			s.buf = append(s.buf[:0], in[i:i+n]...)
			in = in[i+n:]
		case partial:
			s.carry = append(s.carry[:0], in[i:]...)
			in = nil
		default:
			out = append(out, in[i])
			in = in[i+1:]
		}
	}
	return out
}

func (s *syncSmoother) flush() []byte {
	out := append([]byte(nil), s.carry...)
	out = append(out, s.buf...)
	s.carry = nil
	s.buf = nil
	s.active = false
	return out
}

func matchLiteral(p, seq []byte) (n int, partial bool) {
	if len(p) >= len(seq) {
		if bytes.Equal(p[:len(seq)], seq) {
			return len(seq), false
		}
		return 0, false
	}
	return 0, bytes.HasPrefix(seq, p)
}

// matchEnable reports whether p starts with a full target sequence (n is its
// length) or is a proper prefix of one cut short by the chunk end (partial).
func matchEnable(p []byte) (n int, partial bool) {
	for _, seq := range scrubEnableSeqs {
		if len(p) >= len(seq) {
			if bytes.Equal(p[:len(seq)], seq) {
				return len(seq), false
			}
		} else if bytes.HasPrefix(seq, p) {
			partial = true
		}
	}
	if kn, kpartial := matchKittyEnable(p); kn > 0 {
		return kn, false
	} else if kpartial {
		partial = true
	}
	return 0, partial
}

// matchKittyEnable matches the kitty keyboard protocol enables, whose flags
// vary so the literal table cannot hold them: a push (CSI > flags u) or a set
// (CSI = flags ; mode u). Pops (CSI < u) pass through, mirroring the DEC
// disables.
func matchKittyEnable(p []byte) (n int, partial bool) {
	for i, b := range p {
		switch i {
		case 0:
			if b != 0x1b {
				return 0, false
			}
		case 1:
			if b != '[' {
				return 0, false
			}
		case 2:
			if b != '>' && b != '=' {
				return 0, false
			}
		default:
			if b == 'u' {
				return i + 1, false
			}
			if (b < '0' || b > '9') && b != ';' {
				return 0, false
			}
		}
	}
	return 0, true
}
