package hold

import (
	"bytes"
	"sync"
)

// DefaultRingSize is the output ring's capacity: enough raw pty output to
// repaint a reattached terminal (several full screens plus scrollback-style
// logs) without holding a session's whole history in memory.
const DefaultRingSize = 256 * 1024

// Ring is a bounded buffer of the most recent pty output, replayed as BACKLOG
// when a client attaches so the screen repaints instead of coming up blank
// (dtach/abduco keep nothing; blank reattach is their documented weakness).
type Ring struct {
	mu    sync.Mutex
	buf   []byte
	max   int
	trunc bool // older output has been dropped: the front may cut mid-sequence
}

// NewRing returns a ring holding the last max bytes (DefaultRingSize if <= 0).
func NewRing(max int) *Ring {
	if max <= 0 {
		max = DefaultRingSize
	}
	return &Ring{max: max}
}

// Write appends p, keeping only the last max bytes. It never fails; the
// io.Writer shape lets the pty read loop tee into it.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= r.max {
		r.buf = append(r.buf[:0], p[len(p)-r.max:]...)
		r.trunc = true
		return len(p), nil
	}
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.max; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
		r.trunc = true
	}
	return len(p), nil
}

// altEnters are the alternate-screen enter sequences (DEC private modes 1049
// and 47): a full-screen app's clean screen switch, the ideal replay start.
var altEnters = [][]byte{[]byte("\x1b[?1049h"), []byte("\x1b[?47h")}

// homeRedraws are main-screen redraw starts used by terminal apps that do not
// enter the alternate screen. Replaying from the latest one avoids dumping every
// incremental redraw into the viewer after detach/reattach.
var homeRedraws = [][]byte{
	[]byte("\x1b[H"),
	[]byte("\x1b[1;1H"),
	[]byte("\x1b[f"),
}

// Snapshot returns the replayable backlog: the ring contents trimmed to the
// most recent alternate-screen enter when one is present (so a full-screen
// app replays from a clean switch), and otherwise, when older output has been
// dropped, trimmed forward to an escape-sequence boundary so the replay never
// starts mid-sequence and garbles the terminal.
func (r *Ring) Snapshot() []byte {
	r.mu.Lock()
	data := append([]byte(nil), r.buf...)
	trunc := r.trunc
	r.mu.Unlock()

	if i := replayStartIndex(data); i >= 0 {
		return data[i:]
	}
	if !trunc {
		return data
	}
	// The front byte may be the middle of an escape sequence the drop cut in
	// half. Garbling stops at the next fresh start: the next ESC (which begins
	// a new sequence) or just past the next newline, whichever comes first.
	// Plain text with neither is replayed whole (nothing safer exists).
	cut := -1
	if i := bytes.IndexByte(data, 0x1b); i >= 0 {
		cut = i
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 && (cut < 0 || i+1 < cut) {
		cut = i + 1
	}
	if cut < 0 {
		return data
	}
	return data[cut:]
}

// TrimReplayPayload applies the non-destructive replay-boundary part of
// Snapshot to a backlog supplied by another holder process. That lets a newer
// attach client clean up a backlog from an older already-running holder.
func TrimReplayPayload(data []byte) []byte {
	if i := replayStartIndex(data); i >= 0 {
		return data[i:]
	}
	return data
}

func replayStartIndex(data []byte) int {
	if i := lastIndexAny(data, altEnters); i >= 0 {
		return i
	}
	return lastIndexAny(data, homeRedraws)
}

func lastIndexAny(data []byte, seqs [][]byte) int {
	last := -1
	for _, seq := range seqs {
		if i := bytes.LastIndex(data, seq); i > last {
			last = i
		}
	}
	return last
}
