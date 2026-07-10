//go:build windows

package finder

import (
	"os"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows key reader consumes console input records (ReadConsoleInputW)
// instead of a VT byte stream. ENABLE_VIRTUAL_TERMINAL_INPUT is deliberately
// left off: with it on, ConPTY (an ssh session into a Windows host) re-encodes
// keys as VT bytes that the console layer already translated once, corrupting
// or dropping chords and leaking sequence tail bytes as fake keystrokes.
// Reading events gets each key exactly once, with its virtual-key code and
// modifier state intact, and needs no lone-Esc disambiguation timeout.

// termState is the tty's saved input mode: on Windows, the CONIN$ console
// mode word.
type termState struct {
	mode uint32
}

// rawInputMode computes the raw console input mode from an original: echo,
// line buffering, and Ctrl-C processing off (the picker reads every key
// itself), window events on (resize records are the Windows SIGWINCH). VT
// input translation is never enabled; see the package comment above.
func rawInputMode(mode uint32) uint32 {
	mode &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT
	mode |= windows.ENABLE_WINDOW_INPUT
	return mode
}

// makeRawInput arms raw mode on CONIN$ and returns its original mode for
// restoreInput.
func makeRawInput(tty *os.File) (*termState, error) {
	h := windows.Handle(tty.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return nil, err
	}
	if err := windows.SetConsoleMode(h, rawInputMode(mode)); err != nil {
		return nil, err
	}
	return &termState{mode: mode}, nil
}

// reRawInput re-arms raw mode without capturing a new original (the first
// capture from makeRawInput stays the restore point).
func reRawInput(tty *os.File) error {
	h := windows.Handle(tty.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return err
	}
	return windows.SetConsoleMode(h, rawInputMode(mode))
}

// restoreInput puts CONIN$ back into the mode makeRawInput captured.
func restoreInput(tty *os.File, st *termState) {
	if st != nil {
		_ = windows.SetConsoleMode(windows.Handle(tty.Fd()), st.mode)
	}
}

// startInputReader starts the platform key reader: on Windows a console
// input-record loop, bypassing the VT byte parser entirely.
func startInputReader(tty *os.File, out chan ev, stop chan struct{}, done chan struct{}) {
	go readEvents(tty, out, stop, done)
}

// inputRecord is INPUT_RECORD: a WORD event type, 2 bytes of union alignment
// padding, then a 16-byte event union. Declared as [4]uint32 so the struct is
// 4-aligned and a *keyEventRecord cast onto the union is legal.
// https://learn.microsoft.com/en-us/windows/console/input-record-str
type inputRecord struct {
	eventType uint16
	_         uint16
	event     [4]uint32
}

// keyEventRecord is KEY_EVENT_RECORD, the KEY_EVENT arm of the union.
// https://learn.microsoft.com/en-us/windows/console/key-event-record-str
type keyEventRecord struct {
	keyDown         int32
	repeatCount     uint16
	virtualKeyCode  uint16
	virtualScanCode uint16
	unicodeChar     uint16
	controlKeyState uint32
}

// x/sys/windows has the console-mode calls and the VK_*/KEY_EVENT constants
// but no ReadConsoleInputW/WriteConsoleInputW bindings, so those two are
// loaded directly (the same way internal/hold/conpty loads its kernel32
// procs).
var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procReadConsoleInputW  = kernel32.NewProc("ReadConsoleInputW")
	procWriteConsoleInputW = kernel32.NewProc("WriteConsoleInputW")
)

func readConsoleInput(h windows.Handle, recs []inputRecord) (uint32, error) {
	var n uint32
	r, _, err := procReadConsoleInputW.Call(uintptr(h), uintptr(unsafe.Pointer(&recs[0])), uintptr(len(recs)), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return 0, err
	}
	return n, nil
}

func writeConsoleInput(h windows.Handle, rec *inputRecord) error {
	var n uint32
	r, _, err := procWriteConsoleInputW.Call(uintptr(h), uintptr(unsafe.Pointer(rec)), 1, uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return err
	}
	return nil
}

// sentinelScan marks the synthetic key record wakeConsoleInput injects to
// unblock a pending ReadConsoleInputW at shutdown. No real key carries scan
// code 0xFFFF, and the record's VK and char are both zero, so it can never be
// mistaken for input; the reader drops it and re-checks stop.
const sentinelScan = 0xFFFF

// wakeConsoleInput makes a blocked ReadConsoleInputW on the tty return, so the
// reader goroutine can observe its stop channel and exit deterministically
// (CancelIoEx alone is not a reliable wake for a blocked console read).
func wakeConsoleInput(tty *os.File) error {
	rec := inputRecord{eventType: windows.KEY_EVENT}
	k := (*keyEventRecord)(unsafe.Pointer(&rec.event))
	k.keyDown = 1
	k.virtualScanCode = sentinelScan
	return writeConsoleInput(windows.Handle(tty.Fd()), &rec)
}

// winResize fans console resize records out to the active screen's
// watchResize. Package-level and never recreated: the reader is torn down and
// rebuilt around suspend/resume, but the screen watching this channel is not.
var winResize = make(chan struct{}, 1)

func notifyResize() {
	select {
	case winResize <- struct{}{}:
	default:
	}
}

// readEvents is the Windows counterpart of decodeLoop: it reads console input
// records for the process lifetime and translates KEY_EVENT records into the
// same events the unix byte parser emits.
func readEvents(tty *os.File, out chan<- ev, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	h := windows.Handle(tty.Fd())
	var recs [16]inputRecord
	var pendingHigh uint16 // high half of a UTF-16 surrogate pair, awaiting its low half
	for {
		select {
		case <-stop:
			return
		default:
		}
		n, err := readConsoleInput(h, recs[:])
		if err != nil {
			return
		}
		for i := uint32(0); i < n; i++ {
			select {
			case <-stop:
				return
			default:
			}
			switch recs[i].eventType {
			case windows.KEY_EVENT:
				k := (*keyEventRecord)(unsafe.Pointer(&recs[i].event))
				if k.virtualScanCode == sentinelScan {
					continue // shutdown wake-up, not a key
				}
				if k.keyDown == 0 {
					continue
				}
				repeat := int(k.repeatCount)
				if repeat < 1 {
					repeat = 1
				}
				for ; repeat > 0; repeat-- {
					if !emitKeyRecord(out, stop, k, &pendingHigh) {
						return
					}
				}
			case windows.WINDOW_BUFFER_SIZE_EVENT:
				notifyResize()
			}
			// mouse, menu, and focus records are ignored
		}
	}
}

// emitKeyRecord translates one key-down record into the event the unix parser
// would emit for the same key. Keys with a character use it (the console has
// already applied layout and modifiers); character-less keys map by
// virtual-key code.
func emitKeyRecord(out chan<- ev, stop <-chan struct{}, k *keyEventRecord, pendingHigh *uint16) bool {
	state := k.controlKeyState
	ctrl := state&(windows.LEFT_CTRL_PRESSED|windows.RIGHT_CTRL_PRESSED) != 0
	alt := state&(windows.LEFT_ALT_PRESSED|windows.RIGHT_ALT_PRESSED) != 0
	c := k.unicodeChar
	if c == 0 {
		*pendingHigh = 0
		switch k.virtualKeyCode {
		case windows.VK_UP:
			return sendEv(out, stop, ev{t: evUp})
		case windows.VK_DOWN:
			return sendEv(out, stop, ev{t: evDown})
		case windows.VK_LEFT:
			return sendEv(out, stop, ev{t: evLeft})
		case windows.VK_RIGHT:
			return sendEv(out, stop, ev{t: evRight})
		case windows.VK_DELETE, windows.VK_HOME, windows.VK_END, windows.VK_PRIOR, windows.VK_NEXT:
			// recognized so they never leak anything; ax has no events for them
			// (the unix parser drops their VT sequences the same way)
			return true
		}
		if ctrl && k.virtualKeyCode >= 'A' && k.virtualKeyCode <= 'Z' {
			// a Ctrl chord the console gave no character for (Ctrl-Shift-letter
			// on some layouts); the plain Ctrl-letter path below is the usual one
			return sendEv(out, stop, ev{t: evCtrl, r: rune(k.virtualKeyCode - 'A' + 'a')})
		}
		return true // a bare modifier or an unmapped key
	}
	if !utf16.IsSurrogate(rune(c)) {
		*pendingHigh = 0 // a stray unpaired high half is dropped, not emitted
	}
	// Plain Alt is the console's Meta: the unix parser sees ESC then the key,
	// so emit the same pair. Ctrl+Alt is AltGr on international layouts and
	// must pass the character through untouched.
	if alt && !ctrl {
		if !sendEv(out, stop, ev{t: evEsc}) {
			return false
		}
	}
	switch {
	case k.virtualKeyCode == windows.VK_BACK:
		// the Backspace key itself; by character it would be Ctrl-H (0x08) or,
		// with Ctrl held, 0x7f
		return sendEv(out, stop, ev{t: evBack})
	case c == '\r':
		return sendEv(out, stop, ev{t: evEnter})
	case c == '\t':
		return sendEv(out, stop, ev{t: evTab})
	case c == 0x1b:
		return sendEv(out, stop, ev{t: evEsc})
	case c == 0x7f:
		return sendEv(out, stop, ev{t: evBack})
	case c == 3:
		return sendEv(out, stop, ev{t: evCtrlC}) // structural abort, kept distinct
	case c >= 1 && c <= 26:
		return sendEv(out, stop, ev{t: evCtrl, r: rune('a' + c - 1)})
	case utf16.IsSurrogate(rune(c)):
		// a rune above the BMP arrives as two key records, high then low half
		if *pendingHigh != 0 {
			r := utf16.DecodeRune(rune(*pendingHigh), rune(c))
			*pendingHigh = 0
			if r != 0xFFFD {
				return sendEv(out, stop, ev{t: evRune, r: r})
			}
			return true
		}
		*pendingHigh = c
		return true
	case c >= 0x20:
		return sendEv(out, stop, ev{t: evRune, r: rune(c)})
	}
	return true
}
