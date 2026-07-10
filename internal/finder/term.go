package finder

import (
	"bufio"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// evType is a decoded key.
type evType int

const (
	evRune evType = iota
	evEnter
	evEsc
	evTab
	evBack
	evUp
	evDown
	evLeft
	evRight
	evCtrl  // a Ctrl-<letter> chord; the letter is in ev.r (e.g. 'n' for Ctrl-N)
	evCtrlC // kept distinct: the structural abort key
)

// ev is a decoded key. For evRune and evCtrl, r carries the rune (the character,
// or the ctrl'd letter).
type ev struct {
	t evType
	r rune
}

// The keyboard is read once for the whole process, not per screen. ax shows
// several screens in sequence (the picker, then the harness/dir choosers, then a
// prompt), and a per-screen reader left a finished screen's goroutine blocked on
// /dev/tty, stealing the next screen's keys (closing the fd doesn't interrupt a
// blocked tty read). One shared reader, owned for the process lifetime, removes
// the overlap entirely; screens just borrow its event channel.
//
// suspendInput/resumeInput (below) are the one exception: a user keybinding that
// runs a foreground shell command needs the tty to itself (an editor reading
// raw keys would otherwise race the shared reader for the same bytes), so the
// reader is torn down and rebuilt around that command instead of living for the
// whole process.
var (
	inputMu      sync.Mutex
	inputOpen    bool
	sharedTTY    *os.File
	sharedEvents chan ev
	sharedOld    *termState // the tty's mode when the reader opened it, for restore
	sharedStop   chan struct{}
	sharedDone   chan struct{}
	inputErr     error
)

// The output side is likewise opened once for the process. On unix it is just
// another fd on /dev/tty; on Windows it is CONOUT$, which is a different device
// from the CONIN$ the reader owns (writes to CONIN$ fail with "handle is
// invalid", so a single shared fd cannot serve both directions there). It is
// normally left open: suspendInput only tears down the reader, and a still-open
// output fd is harmless to a child that borrows the terminal. Windows exec
// handoff is the exception: that closes the CONOUT$ handle after restoring its
// mode so the fake-exec child inherits only its stdio handles.
var (
	outputMu   sync.Mutex
	outputOpen bool
	sharedOut  *os.File
	outputErr  error
)

func sharedOutput() (*os.File, error) {
	outputMu.Lock()
	defer outputMu.Unlock()
	if !outputOpen {
		sharedOut, outputErr = openTTYOut()
		outputOpen = outputErr == nil
	}
	if outputOpen {
		prepareTTYOut(sharedOut)
	}
	return sharedOut, outputErr
}

func sharedInput() (*os.File, chan ev, error) {
	inputMu.Lock()
	defer inputMu.Unlock()
	if !inputOpen {
		sharedTTY, inputErr = openTTY()
		if inputErr == nil {
			// Raw mode must be set BEFORE the reader issues its first read, so no
			// key is ever consumed under the cooked mode. We capture the original
			// mode here (once per tty open) so the per-screen restore below leaves
			// the terminal exactly as we found it, even though each screen re-arms
			// raw independently. makeRawInput/startInputReader are the platform
			// seam: unix arms termios raw and decodes a VT byte stream
			// (decodeLoop); Windows arms console raw (no VT input translation) and
			// reads input records (readEvents).
			sharedOld, inputErr = makeRawInput(sharedTTY)
			if inputErr != nil {
				sharedTTY.Close()
				sharedTTY = nil
				return sharedTTY, sharedEvents, inputErr
			}
			sharedEvents = make(chan ev, 32)
			sharedStop = make(chan struct{})
			sharedDone = make(chan struct{})
			startInputReader(sharedTTY, sharedEvents, sharedStop, sharedDone)
			inputOpen = true
		}
	}
	return sharedTTY, sharedEvents, inputErr
}

// suspendInput closes the shared tty, freeing the terminal for a child process
// to own exclusively until resumeInput opens a fresh reader. It waits (bounded)
// for the reader goroutine to exit so resumeInput never starts a second reader
// racing the old one for keys.
func suspendInput() {
	shutdownInput()
}

// ShutdownInputForExec permanently hands the terminal to a child process about
// to be fake-exec'd. On Unix execReplace is a real execve and this is not used;
// on Windows the parent process survives behind the child, so its process-wide
// CONIN$ reader must be stopped before the attach client starts reading detach
// chords.
func ShutdownInputForExec() {
	shutdownInput()
	closeOutputForExec()
}

// inputShutdownTimeout bounds shutdownInput's wait for the reader goroutine to
// exit. The unblock (a sentinel input record on Windows, fd close on unix) is
// best-effort: a console or tty read the platform cannot abort would otherwise
// pin the wait forever, freezing the picker at the exact moment it hands the
// terminal over. A reader that outlives the bound is harmless (see
// waitReaderExit), so teardown proceeds without it.
const inputShutdownTimeout = 250 * time.Millisecond

// waitReaderExit waits for the reader goroutine to close done, giving up after
// inputShutdownTimeout. A reader that misses the bound is still blocked in its
// final read; it holds only its own dead state (the closed stop channel, the
// closed tty, the abandoned events channel nobody reads), so when that read
// finally returns it observes stop and exits without touching any reader
// started since. Reports whether the reader exited within the bound.
func waitReaderExit(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-time.After(inputShutdownTimeout):
		return false
	}
}

func shutdownInput() {
	var done chan struct{}
	inputMu.Lock()
	if inputOpen {
		restoreInput(sharedTTY, sharedOld)
		if sharedStop != nil {
			close(sharedStop)
		}
		cancelTTYRead(sharedTTY)
		sharedTTY.Close()
		done = sharedDone
		inputOpen = false
		sharedTTY = nil
		sharedEvents = nil
		sharedOld = nil
		sharedStop = nil
		sharedDone = nil
		inputErr = nil
	}
	inputMu.Unlock()
	if done != nil {
		waitReaderExit(done)
	}
}

// resumeInput reopens the tty and restarts decodeLoop after a suspendInput,
// handing the picker a fresh events channel. The old channel stays with the
// old reader; if that reader outlived suspendInput's bounded wait, a last key
// it stole lands in the old channel's buffer, which nobody reads.
func resumeInput() (*os.File, chan ev, error) {
	return sharedInput()
}

// screen owns the alternate screen and raw mode for one interaction; it shares
// the process keyboard reader and renders full frames. tty is the input side
// (raw mode, key reads); out is the output side (frames, cursor control,
// measurement). On unix both are /dev/tty; on Windows they are CONIN$/CONOUT$.
type screen struct {
	tty    *os.File
	out    *os.File
	old    *termState
	rows   int
	cols   int
	events chan ev
	resize chan struct{}
	done   chan struct{}
}

// reportingModeResets defensively disables the terminal reporting modes a
// harness run inside a held session may have turned on: mouse tracking
// (1000 click, 1002 button-drag, 1003 any-motion) and its SGR encoding (1006),
// focus reporting (1004), bracketed paste (2004), and Windows win32-input-mode
// (9001). A raw-mode restore does not touch these DEC/private modes, so a
// harness that left one on keeps the real terminal spewing mouse/focus/paste or
// Windows key-event reports at the shell prompt after ax hands the tty back.
// Sending the disable is a no-op when the mode was never on, so it is safe to
// emit on every teardown.
const reportingModeResets = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?2004l\x1b[?9001l"

// screenTeardown is what close/suspend write to return the terminal to the
// shell: restore autowrap, show the cursor, leave the alt screen, then clear any
// reporting modes a harness left on. The order matters on Windows, where these
// bytes must go out while VT processing is still on (before restoreTTYOut).
const screenTeardown = "\x1b[?7h\x1b[?25h\x1b[?1049l" + reportingModeResets

const screenEnter = "\x1b[0m\x1b[?1049h\x1b[?25l\x1b[?7l\x1b[H\x1b[2J"

type terminalSize struct {
	cols int
	rows int
}

func chooseTerminalSize(candidates ...terminalSize) (int, int, bool) {
	best := terminalSize{}
	for _, c := range candidates {
		if c.cols <= 0 || c.rows <= 0 {
			continue
		}
		if best.cols == 0 || c.cols > best.cols || (c.cols == best.cols && c.rows > best.rows) {
			best = c
		}
	}
	if best.cols == 0 {
		return 0, 0, false
	}
	return best.cols, best.rows, true
}

func openScreen() (*screen, error) {
	tty, events, err := sharedInput()
	if err != nil {
		return nil, err
	}
	out, err := sharedOutput()
	if err != nil {
		return nil, err
	}
	// sharedInput already armed raw mode and captured the tty's original mode
	// (sharedOld) before the reader's first read. Re-arm raw here in case a prior
	// screen's close restored the original, but restore from that captured
	// original rather than re-capturing (which would snapshot the already-raw mode
	// and leave the terminal raw on exit).
	if err := reRawInput(tty); err != nil {
		return nil, err
	}
	s := &screen{tty: tty, out: out, old: sharedOld, events: events, resize: make(chan struct{}, 1), done: make(chan struct{})}
	s.measure()
	// Alt screen, reset attributes, hide cursor, disable autowrap (so an
	// over-long line clips at the edge instead of wrapping and scrolling the
	// frame), home, clear.
	s.out.WriteString(screenEnter)
	s.watchResize()
	return s, nil
}

func (s *screen) close() {
	close(s.done)
	s.out.WriteString(screenTeardown) // restore wrap, show cursor, leave alt screen, clear reporting modes
	restoreInput(s.tty, s.old)
	restoreSharedOutputMode()
	// the shared tty and its reader stay alive for the next screen
}

// suspend leaves the alt screen and raw mode and releases the shared tty
// reader, so a child process (a user keybinding's shell command) can read the
// terminal directly with no second reader racing it for input.
func (s *screen) suspend() {
	s.out.WriteString(screenTeardown)
	restoreInput(s.tty, s.old)
	restoreSharedOutputMode()
	suspendInput()
}

// resume undoes suspend: reopens the tty, restarts the reader, re-enters raw
// mode and the alt screen, and re-measures in case the child resized it.
// Returns an error only if the tty cannot be reopened, in which case the
// picker cannot continue and should give up rather than render into nothing.
func (s *screen) resume() error {
	tty, events, err := resumeInput()
	if err != nil {
		return err
	}
	// resumeInput reopened the tty and re-armed raw (capturing the fresh original
	// into sharedOld) before its reader's first read, the same ordering openScreen
	// relies on; re-arm here too and restore from that captured original.
	if err := reRawInput(tty); err != nil {
		return err
	}
	prepareSharedOutputMode()
	s.tty, s.old, s.events = tty, sharedOld, events
	s.measure()
	s.out.WriteString(screenEnter)
	return nil
}

func (s *screen) measure() {
	c, r, err := measureTTYSize(s.out)
	if err != nil || c == 0 || r == 0 {
		s.cols, s.rows = 80, 24
		return
	}
	s.cols, s.rows = c, r
}

// render writes the frame: home, each line cleared to EOL, then clear below.
func (s *screen) render(lines []string) {
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, ln := range lines {
		b.WriteString(ln)
		b.WriteString("\x1b[K")
		if i < len(lines)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\x1b[0J")
	s.out.WriteString(b.String())
}

func restoreSharedOutputMode() {
	outputMu.Lock()
	defer outputMu.Unlock()
	if outputOpen {
		restoreTTYOut(sharedOut)
	}
}

func prepareSharedOutputMode() {
	outputMu.Lock()
	defer outputMu.Unlock()
	if outputOpen {
		prepareTTYOut(sharedOut)
	}
}

func closeOutputForExec() {
	outputMu.Lock()
	defer outputMu.Unlock()
	if !outputOpen {
		return
	}
	restoreTTYOut(sharedOut)
	sharedOut.Close()
	sharedOut = nil
	outputOpen = false
	outputErr = nil
}

// decodeLoop reads raw bytes for the process lifetime and decodes them into key
// events. An inner goroutine does the blocking reads so the escape-sequence
// timeout (lone Esc vs arrow keys) can be a channel select.
func decodeLoop(tty *os.File, out chan<- ev, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	r := bufio.NewReader(tty)
	bytes := make(chan byte, 256)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer close(bytes)
		for {
			b, err := r.ReadByte()
			if err != nil {
				return
			}
			select {
			case bytes <- b:
			case <-stop:
				return
			}
		}
	}()
	defer func() { <-readDone }()
	decodeBytes(bytes, out, stop)
}

// escSeqTimeout bounds every read inside an in-flight escape sequence: the same
// window that tells a lone Esc from the start of a sequence also caps each
// following byte, so a truncated sequence is dropped instead of eating the next
// real keypress.
const escSeqTimeout = 40 * time.Millisecond

type byteStatus int

const (
	byteOK byteStatus = iota
	byteTimeout
	byteClosed // stop closed or the byte stream ended
)

// nextByte pulls the next byte of an in-flight escape sequence, waiting at most
// escSeqTimeout.
func nextByte(bytes <-chan byte, stop <-chan struct{}) (byte, byteStatus) {
	select {
	case b, ok := <-bytes:
		if !ok {
			return 0, byteClosed
		}
		return b, byteOK
	case <-time.After(escSeqTimeout):
		return 0, byteTimeout
	case <-stop:
		return 0, byteClosed
	}
}

// decodeBytes is the decoder core, split from decodeLoop so tests can feed it a
// byte channel directly.
func decodeBytes(bytes <-chan byte, out chan<- ev, stop <-chan struct{}) {
	for {
		var b byte
		var ok bool
		select {
		case b, ok = <-bytes:
		case <-stop:
			return
		}
		if !ok {
			return
		}
		if b == 0x1b { // escape: a key sequence, an Alt chord, or a lone Esc
			if !decodeEscape(bytes, out, stop) {
				return
			}
			continue
		}
		if b >= 0x80 { // a UTF-8 multibyte rune (typed in a query)
			if r := decodeRune(b, bytes); r != 0 {
				if !sendEv(out, stop, ev{t: evRune, r: r}) {
					return
				}
			}
			continue
		}
		if !emitKey(out, stop, b) {
			return
		}
	}
}

// decodeEscape handles the stream after a 0x1b: a CSI or SS3 key sequence, a
// string sequence to discard (OSC/DCS/SOS/PM/APC), an Alt-chorded key, or a
// lone Esc when nothing follows within the disambiguation window. Reports false
// when the reader must exit.
func decodeEscape(bytes <-chan byte, out chan<- ev, stop <-chan struct{}) bool {
	b, st := nextByte(bytes, stop)
	switch st {
	case byteClosed:
		return false
	case byteTimeout: // a lone Esc keypress
		return sendEv(out, stop, ev{t: evEsc})
	}
	switch b {
	case '[':
		return decodeCSI(bytes, out, stop)
	case 'O': // SS3: arrows in application cursor mode; F1-F4 finals drop
		b2, st := nextByte(bytes, stop)
		if st == byteClosed {
			return false
		}
		if st == byteTimeout { // truncated: drop
			return true
		}
		return emitArrow(out, stop, b2)
	case ']', 'P', 'X', '^', '_': // OSC/DCS/SOS/PM/APC: consume to BEL/ST, drop
		return skipStringSeq(bytes, stop)
	default: // Alt-<key> arrives as Esc then the key
		if !sendEv(out, stop, ev{t: evEsc}) {
			return false
		}
		return emitKey(out, stop, b)
	}
}

// decodeCSI consumes one full CSI sequence: parameter bytes (0x30-0x3F),
// intermediate bytes (0x20-0x2F), then a single final (0x40-0x7E). Only the
// finals ax honors become events; everything else (Delete/Home/End/PgUp/PgDn
// and F-key tilde sequences, bracketed-paste markers, mouse reports, unknown
// finals) is consumed whole so no parameter or tail byte ever leaks as a typed
// rune. Reports false when the reader must exit.
func decodeCSI(bytes <-chan byte, out chan<- ev, stop <-chan struct{}) bool {
	nparams := 0
	for {
		b, st := nextByte(bytes, stop)
		if st == byteClosed {
			return false
		}
		if st == byteTimeout { // truncated: drop what arrived
			return true
		}
		switch {
		case b >= 0x30 && b <= 0x3F: // parameter bytes: digits ; : < = > ?
			nparams++
		case b >= 0x20 && b <= 0x2F: // intermediate bytes
		case b >= 0x40 && b <= 0x7E: // final byte: dispatch
			switch b {
			case 'A', 'B', 'C', 'D': // arrows, plain or modified (ESC[A, ESC[1;5A)
				return emitArrow(out, stop, b)
			case 'M':
				if nparams == 0 { // X10 mouse report: three payload bytes follow
					for i := 0; i < 3; i++ {
						if _, st := nextByte(bytes, stop); st == byteClosed {
							return false
						} else if st == byteTimeout {
							return true
						}
					}
				}
				return true // SGR mouse press (ESC[<...M): drop
			}
			return true // tilde finals, H/F, SGR mouse release m, unknown: drop
		default:
			return true // control byte mid-sequence: malformed, drop
		}
	}
}

// skipStringSeq consumes an OSC/DCS/SOS/PM/APC string body through its BEL or
// ST (ESC \) terminator and drops it. Reports false when the reader must exit.
func skipStringSeq(bytes <-chan byte, stop <-chan struct{}) bool {
	for {
		b, st := nextByte(bytes, stop)
		if st == byteClosed {
			return false
		}
		if st == byteTimeout { // truncated: drop what arrived
			return true
		}
		if b == 0x07 { // BEL, OSC's common terminator
			return true
		}
		if b == 0x1b { // possible ST
			b2, st := nextByte(bytes, stop)
			if st == byteClosed {
				return false
			}
			if st == byteTimeout || b2 == '\\' {
				return true
			}
		}
	}
}

// decodeRune assembles a full UTF-8 rune from a lead byte and its continuation
// bytes pulled off the stream.
func decodeRune(lead byte, bytes <-chan byte) rune {
	n := 1
	switch {
	case lead&0xE0 == 0xC0:
		n = 2
	case lead&0xF0 == 0xE0:
		n = 3
	case lead&0xF8 == 0xF0:
		n = 4
	}
	buf := make([]byte, 1, 4)
	buf[0] = lead
	for i := 1; i < n; i++ {
		c, ok := <-bytes
		if !ok {
			return 0
		}
		buf = append(buf, c)
	}
	if r, _ := utf8.DecodeRune(buf); r != utf8.RuneError {
		return r
	}
	return 0
}

// evToKey renders a decoded event as the key string the keymap matches against
// (the same notation config uses): a rune as itself, a ctrl chord as "ctrl-x",
// and the named specials. Returns "" for events with no key notation.
func evToKey(e ev) string {
	switch e.t {
	case evRune:
		return string(e.r)
	case evCtrl:
		return "ctrl-" + string(e.r)
	case evTab:
		return "tab"
	case evEnter:
		return "enter"
	case evEsc:
		return "esc"
	case evBack:
		return "backspace"
	case evUp:
		return "up"
	case evDown:
		return "down"
	case evLeft:
		return "left"
	case evRight:
		return "right"
	case evCtrlC:
		return "ctrl-c"
	}
	return ""
}

func sendEv(out chan<- ev, stop <-chan struct{}, e ev) bool {
	select {
	case out <- e:
		return true
	case <-stop:
		return false
	}
}

func emitArrow(out chan<- ev, stop <-chan struct{}, b byte) bool {
	switch b {
	case 'A':
		return sendEv(out, stop, ev{t: evUp})
	case 'B':
		return sendEv(out, stop, ev{t: evDown})
	case 'C':
		return sendEv(out, stop, ev{t: evRight})
	case 'D':
		return sendEv(out, stop, ev{t: evLeft})
	}
	return true
}

func emitKey(out chan<- ev, stop <-chan struct{}, b byte) bool {
	switch {
	case b == '\r':
		return sendEv(out, stop, ev{t: evEnter})
	case b == '\t':
		return sendEv(out, stop, ev{t: evTab})
	case b == 0x7f:
		return sendEv(out, stop, ev{t: evBack})
	case b == 3:
		return sendEv(out, stop, ev{t: evCtrlC}) // structural abort, kept distinct
	case b >= 1 && b <= 26:
		// any other Ctrl-<letter>: byte 1 is Ctrl-A ... byte 26 is Ctrl-Z.
		return sendEv(out, stop, ev{t: evCtrl, r: rune('a' + b - 1)})
	case b >= 0x20 && b < 0x7f:
		return sendEv(out, stop, ev{t: evRune, r: rune(b)})
	}
	return true
}
