package hold

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/config"
)

// The attach client is cross-platform: this file is the whole client except
// four seams the platforms fill in (client_unix.go, client_windows.go):
// rawTerminal (termios raw mode vs SetConsoleMode), watchResize (SIGWINCH vs
// a console size poll), notifyDetach (which signals mean "window closed"),
// and cleanStale (unix sockets leave stale files; pipes cannot).

// failGrace mirrors the run wrapper's launch-failure rule: a non-zero exit
// within this of launch is a failed launch worth holding on screen; a longer
// run's non-zero exit is a session ending and the client just exits.
const failGrace = 15 * time.Second

// failHoldTimeout auto-dismisses a held failure, so a client whose human
// walked away does not pin a window forever.
const failHoldTimeout = 15 * time.Minute

// spawnWait bounds how long the client waits for a freshly spawned holder's
// socket to answer. Holder startup is fast (fork + pty + listen); a holder
// that cannot answer in this long died on launch.
const spawnWait = 5 * time.Second

// helloWait bounds the wait for the holder's HELLO after ATTACH, so attaching
// to a socket that is not a native holder (a dtach master from an older ax)
// reports the mismatch instead of hanging.
const helloWait = 3 * time.Second

// detachCloseGrace is a best-effort window for sending DETACH before forcibly
// closing the connection. A wedged write must not keep the pane attached.
const detachCloseGrace = 100 * time.Millisecond

const clientWriteQueue = 64

// reportingModes are the DEC/private mode numbers that must not survive ax:
// mouse tracking (1000 click, 1002 button-drag, 1003 any-motion) and its SGR
// encoding (1006), focus reporting (1004), bracketed paste (2004), and
// Windows Terminal win32-input-mode (9001). ReportingModeReset disables them all
// on teardown. The output scrubber (scrub.go) strips only the win32-input enable
// from this set (scrubbedModes); the rest pass through to the terminal while
// attached so the harness gets scroll/focus/paste, and the teardown reset clears
// them so nothing leaks to the shell.
var reportingModes = []string{"1000", "1002", "1003", "1006", "1004", "2004", "9001"}

// ReportingModeReset defensively disables the terminal modes the held harness
// may have turned on. These modes live in the real terminal (the harness's
// enable sequences crossed the wire and reached it), and a raw-mode restore
// does not touch them, so without this the terminal keeps emitting
// mouse/focus/paste or Windows key-event reports at the shell prompt after a
// detach. Sending the disable is a no-op when the mode was never on, so it is
// safe on every teardown path. Exported so the run wrapper can reuse the same
// reset instead of duplicating the escape string.
var ReportingModeReset = func() string {
	var b strings.Builder
	for _, m := range reportingModes {
		b.WriteString("\x1b[?" + m + "l")
	}
	// Also disarm the keyboard protocols a harness may have turned on in the
	// real terminal (the scrubber strips the enables from an attach stream,
	// but an in-window run's output reaches the terminal directly): xterm
	// modifyOtherKeys back to level 0 and one kitty keyboard protocol pop.
	// A terminal armed with either encodes Ctrl-A as an escape sequence
	// instead of 0x01, which would kill the next attach's detach chord. Both
	// resets are no-ops on a terminal that never armed them.
	b.WriteString("\x1b[>4;0m\x1b[<u")
	return b.String()
}()

// ansiRe strips terminal escape sequences from a replayed failure tail, so it
// prints as text instead of re-driving the terminal. (Same shape as the run
// wrapper's; the tail crossed the wire raw.)
var ansiRe = regexp.MustCompile(`\x1b(\][^\a\x1b]*(\a|\x1b\\)|\[[0-9;?<>=$ ]*[a-zA-Z@]|[()][A-Z0-9]|.)`)

type clientWrite struct {
	typ        byte
	payload    []byte
	closeAfter bool
}

type clientWriter struct {
	conn      net.Conn
	q         chan clientWrite
	closed    chan struct{}
	closeOnce sync.Once
}

func newClientWriter(conn net.Conn) *clientWriter {
	w := &clientWriter{
		conn:   conn,
		q:      make(chan clientWrite, clientWriteQueue),
		closed: make(chan struct{}),
	}
	go w.loop()
	return w
}

func (w *clientWriter) loop() {
	for {
		select {
		case frame := <-w.q:
			if err := writeFrameDeadline(w.conn, frame.typ, frame.payload, writeTimeout); err != nil {
				w.close()
				return
			}
			if frame.closeAfter {
				w.close()
				return
			}
		case <-w.closed:
			return
		}
	}
}

func (w *clientWriter) send(typ byte, payload []byte) bool {
	select {
	case <-w.closed:
		return false
	default:
	}
	frame := clientWrite{typ: typ, payload: append([]byte(nil), payload...)}
	select {
	case w.q <- frame:
		return true
	case <-w.closed:
		return false
	default:
		w.close()
		return false
	}
}

func (w *clientWriter) detach() {
	select {
	case w.q <- clientWrite{typ: MsgDetach, closeAfter: true}:
	case <-w.closed:
	default:
	}
	time.AfterFunc(detachCloseGrace, w.close)
}

func (w *clientWriter) close() {
	w.closeOnce.Do(func() {
		close(w.closed)
		w.conn.Close()
	})
}

// Attach is the native attach client: it connects to session id's holder
// (spawning one via spawn when none is running: dtach -A create-or-attach
// semantics), puts the terminal in raw mode, and multiplexes it with the held
// harness until detach (the Ctrl-A then d chord by default, return 0) or
// harness exit (return its code). spawn == nil means attach-only.
// openMenu, when non-nil, is invoked after a menu-chord detach has fully torn
// the client down (terminal restored, holder still alive): it reopens the ax
// picker in this terminal (an exec-replace, so it does not return on success).
// nil means the caller has no picker to open (the menu chord then falls back to
// a plain detach).
func Attach(id string, spawn func() error, openMenu func()) (int, error) {
	cfg, _ := config.Load()
	prefix := DetachPrefixByte(cfg.DetachPrefix)
	letter := DetachLetterByte(cfg.DetachKey)
	menuLetter := MenuLetterByte(cfg.MenuKey)
	if _, ok := ParseDetachKey(cfg.DetachPrefix); cfg.DetachPrefix != "" && !ok {
		axlog.Printf("attach %s: bad detach_prefix %q; using ctrl-a", id, cfg.DetachPrefix)
	}
	if _, ok := ParseDetachLetter(cfg.DetachKey); cfg.DetachKey != "" && !ok {
		axlog.Printf("attach %s: bad detach_key %q; using d", id, cfg.DetachKey)
	}
	if _, ok := ParseDetachLetter(cfg.MenuKey); cfg.MenuKey != "" && !ok {
		axlog.Printf("attach %s: bad menu_key %q; using a", id, cfg.MenuKey)
	}
	conn, err := dialOrCreate(id, spawn)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	stdinFd := int(os.Stdin.Fd())
	rows, cols := uint16(40), uint16(120)
	isTerm := term.IsTerminal(stdinFd)
	if isTerm {
		if r, c, ok := TerminalSize(stdinFd); ok {
			rows, cols = r, c
		}
	}
	if err := writeFrameDeadline(conn, MsgAttach, EncodeAttach(rows, cols), writeTimeout); err != nil {
		return 0, fmt.Errorf("attach %s: %w", id, err)
	}
	conn.SetReadDeadline(time.Now().Add(helloWait))
	typ, payload, err := ReadFrame(conn)
	if err != nil || typ != MsgHello {
		return 0, fmt.Errorf("attach %s: no HELLO from the holder (a session held by an older ax? reattach with dtach, or `ax restart` it)", id)
	}
	conn.SetReadDeadline(time.Time{})
	proto, _, err := DecodeHello(payload)
	if err != nil {
		return 0, fmt.Errorf("attach %s: %w", id, err)
	}
	if proto != Proto {
		return 0, fmt.Errorf("attach %s: holder speaks protocol %d, this ax speaks %d; reattach with the ax that launched it or restart the session", id, proto, Proto)
	}
	writer := newClientWriter(conn)
	defer writer.close()

	var restoreOnce sync.Once
	restore := func() {}
	if isTerm {
		if r, err := rawTerminal(stdinFd); err == nil {
			restore = r
		} else {
			axlog.Printf("attach %s: raw mode: %v", id, err)
		}
		// Wrap the platform restore so every teardown path also clears any
		// terminal reporting modes the held harness left on (mouse/focus/paste),
		// which the raw-mode restore alone does not touch. The reset must go out
		// before the platform restore because on Windows rawTerminal enabled VT
		// processing on stdout and its restore turns it back off; emitting after
		// would print the escape bytes literally. Guarded by isTerm so a
		// redirected stdout never gets escape bytes written to it.
		inner := restore
		restore = func() {
			os.Stdout.WriteString(ReportingModeReset)
			inner()
		}
	}
	defer restoreOnce.Do(restore)

	// Keep the holder's pty sized to this terminal (SIGWINCH-driven on unix, a
	// console size poll on Windows, which has no resize signal).
	stopResize := watchResize(writer.send, stdinFd)
	defer stopResize()

	// Detach is one shared, once-only path (the byte scan and the signal
	// handler must not double-close the conn or race the DETACH frame). It
	// fires from the chord scan below (the configured prefix then letter,
	// Ctrl-A then d by default, plus the Ctrl-backslash fallback), from the menu
	// chord (Ctrl-A then a, which detaches identically and then reopens the
	// picker), and from the platform's window-closed/kill signals (notifyDetach).
	// Either way the holder lives on.
	detached := new(atomic.Bool)
	// menu records that the detach was a menu chord (Ctrl-A then a), so the
	// teardown below reopens the picker instead of returning to the shell. It is
	// set before detach() so the reader goroutine and the read loop agree.
	menu := new(atomic.Bool)
	var detachOnce sync.Once
	detach := func() {
		detachOnce.Do(func() {
			detached.Store(true)
			writer.detach()
		})
	}
	stopSig := notifyDetach(detach)
	defer stopSig()

	// Forward keystrokes, scanning for the detach chord. The pending state
	// lives across reads: a prefix arriving as the last byte of one read and
	// its command letter at the start of the next still chords. Once the
	// harness has exited and the failure is held on screen, keystrokes become
	// the dismiss signal instead (single reader, so the keypress cannot be
	// lost).
	holding := new(atomic.Bool)
	dismiss := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 1024)
		pending := false
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if holding.Load() {
					select {
					case dismiss <- struct{}{}:
					default:
					}
				} else {
					var fwd []byte
					var act chordAction
					fwd, act, pending = scanChord(buf[:n], prefix, letter, menuLetter, pending)
					if len(fwd) > 0 {
						writer.send(MsgInput, fwd)
					}
					if act != chordNone {
						// The menu chord shares the detach teardown; it only sets
						// menu first so the read loop reopens the picker after.
						if act == chordMenu {
							menu.Store(true)
						}
						detach()
						return
					}
				}
			}
			if err != nil {
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				if holding.Load() {
					select {
					case dismiss <- struct{}{}:
					default:
					}
				}
				detach()
				return
			}
		}
	}()

	// Held output replays whatever the inner terminal rendered, including its own
	// mode-enables. Most pass through: while attached the user is driving the
	// harness, so mouse tracking (the scroll wheel), focus reporting and bracketed
	// paste should reach it, and ReportingModeReset clears them at detach/exit.
	// The scrubber strips only what would break the terminal for ax itself:
	// win32-input-mode (9001) and the keyboard-protocol enables, which re-encode
	// input and would change what Ctrl-A produces (the detach chord). It carries
	// partial sequences across frame boundaries, over both backlog replay and the
	// live stream.
	scrubber := &TerminalModeScrubber{}
	hinted := false
	for {
		typ, payload, err := ReadFrame(conn)
		if err != nil {
			if !detached.Load() {
				if out := scrubber.Flush(); len(out) > 0 {
					os.Stdout.Write(out)
				}
			}
			restoreOnce.Do(restore)
			if detached.Load() {
				// A menu-chord detach reopens the picker in this terminal instead
				// of returning to the shell. openMenu exec-replaces on success, so
				// it does not return; if it fails (or is nil) fall through to the
				// ordinary detach message: the session is detached either way.
				if menu.Load() && openMenu != nil {
					openMenu()
				}
				fmt.Printf("\r\n[ax: detached; the session keeps running (`ax attach %s` to return)]\r\n", id)
				return 0, nil
			}
			// The holder vanished without an EXIT: a crash. The harness itself
			// survives orphaned on unix; the stale heartbeat is the crash signal.
			fmt.Printf("\r\n[ax: connection to the holder lost]\r\n")
			return 0, nil
		}
		switch typ {
		case MsgBacklog:
			// Repaint: clear, replay the ring, and let the holder's winch nudge
			// make a full-screen harness redraw the rest.
			os.Stdout.WriteString("\x1b[2J\x1b[H")
			os.Stdout.Write(scrubber.Scrub(TrimReplayPayload(payload)))
			// The discoverability hint rides after the replay (which just cleared
			// the screen, so printing it earlier would wipe it): a full-screen
			// harness paints over it on the nudge, a scrolling one keeps it.
			if !hinted && isTerm {
				hinted = true
				fmt.Printf("\r\n\x1b[2m[ax: attached; %s to detach, %s for the picker]\x1b[0m\r\n", DetachLabel(cfg.DetachPrefix, cfg.DetachKey), MenuLabel(cfg.DetachPrefix, cfg.MenuKey))
			}
		case MsgOutput:
			os.Stdout.Write(scrubber.Scrub(payload))
		case MsgExit:
			code, runtime, tail, derr := DecodeExit(payload)
			if derr != nil {
				restoreOnce.Do(restore)
				return 0, nil
			}
			if out := scrubber.Flush(); len(out) > 0 {
				os.Stdout.Write(out)
			}
			// A fresh non-zero exit is a failed launch; hold it on screen until a
			// keypress so the window does not flash away with the reason unread.
			if code != 0 && runtime < failGrace && isTerm {
				holding.Store(true)
				holdFailure(code, runtime, tail, dismiss)
			}
			restoreOnce.Do(restore)
			if code != 0 && (runtime >= failGrace || !isTerm) {
				fmt.Printf("\r\n[ax: session exited with code %d]\r\n", code)
			}
			return code, nil
		}
	}
}

// TerminalSize returns the best rows/cols for the user's terminal. It is shared
// by attach and the Windows run wrapper so a ConPTY starts and resizes against
// the visible terminal, not a stale default.
func TerminalSize(fd int) (rows, cols uint16, ok bool) {
	return terminalSize(fd)
}

// chordAction is what an armed prefix resolved into: nothing yet, a plain
// detach (return to the shell), or a menu detach (detach, then reopen the ax
// picker). Both detach kinds tear the client down identically; only what
// happens after the client exits differs.
type chordAction int

const (
	chordNone chordAction = iota
	chordDetach
	chordMenu
)

// scanChord runs the chord state machine over one read: bytes flow through to
// fwd untouched except the prefix, which arms pending instead of forwarding.
// The byte after an armed prefix decides: the detach letter detaches (neither
// byte forwarded), the menu letter detaches-then-reopens-the-picker, the prefix
// again forwards exactly one literal prefix (press it twice to type it),
// anything else forwards the held prefix then that byte, lossless.
// pendingIn/pendingOut carry the armed state across reads, so a prefix split
// from its letter by a read boundary still chords. The Ctrl-backslash fallback
// detaches anywhere, armed or not. On a chord the rest of the read is dropped,
// never forwarded.
func scanChord(p []byte, prefix, letter, menu byte, pendingIn bool) (fwd []byte, act chordAction, pendingOut bool) {
	pending := pendingIn
	for _, b := range p {
		if pending {
			pending = false
			switch b {
			case letter, detachFallback:
				return fwd, chordDetach, false
			case menu:
				return fwd, chordMenu, false
			case prefix:
				fwd = append(fwd, prefix)
			default:
				fwd = append(fwd, prefix, b)
			}
			continue
		}
		switch b {
		case prefix:
			pending = true
		case detachFallback:
			return fwd, chordDetach, false
		default:
			fwd = append(fwd, b)
		}
	}
	return fwd, chordNone, pending
}

// holdFailure prints why the harness died and blocks until dismissed, the
// client-side successor of the run wrapper's in-window failure hold (the
// holder is detached now, so the client owns the terminal that shows it).
// SIGTERM/SIGHUP dismiss too; Go delivers SIGTERM for a closing Windows
// console as well, so the constants are portable.
func holdFailure(code int, runtime time.Duration, tail []byte, dismiss <-chan struct{}) {
	last := strings.TrimSpace(ansiRe.ReplaceAllString(string(tail), ""))
	fmt.Printf("\r\n\x1b[31max: harness exited with code %d after %s\x1b[0m\r\n", code, runtime.Round(time.Millisecond))
	if last != "" {
		for _, line := range strings.Split(last, "\n") {
			fmt.Printf("  %s\r\n", strings.TrimRight(line, "\r"))
		}
	}
	fmt.Printf("\x1b[2msee `ax log`; press any key to close\x1b[0m\r\n")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	select {
	case <-dismiss:
	case <-sig:
	case <-time.After(failHoldTimeout):
	}
}

// dialOrCreate implements dtach -A semantics: attach to a live holder, and
// when there is none (no socket, or a stale file whose holder died) spawn one
// and wait for its socket to answer.
func dialOrCreate(id string, spawn func() error) (net.Conn, error) {
	conn, err := dial(id)
	if err == nil {
		return conn, nil
	}
	if spawn == nil {
		return nil, fmt.Errorf("no holder answering for session %s: %v", id, err)
	}
	cleanStale(id, err)
	if err := spawn(); err != nil {
		return nil, fmt.Errorf("start holder for %s: %w", id, err)
	}
	deadline := time.Now().Add(spawnWait)
	for {
		conn, err = dial(id)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("holder for %s did not come up: %v (see `ax log`)", id, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
