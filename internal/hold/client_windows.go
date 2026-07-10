//go:build windows

package hold

import (
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// resizePoll is how often the Windows client compares its console size against
// the last one sent: Windows has no SIGWINCH, so resize is a poll (the design
// doc's ~4/s).
const resizePoll = 250 * time.Millisecond

// rawTerminal puts the client's console into raw passthrough mode: line input,
// processed input (Ctrl-C as a signal), and echo off so every keystroke (and
// the detach chord) arrives byte-by-byte, VT input on so arrow keys and the
// like arrive as escape sequences the harness understands. Stdout additionally
// gets VT processing (x/term's MakeRaw does not set it) so the holder's raw
// ANSI stream renders, and newline auto-return off because the ConPTY stream
// carries its own \r\n. Returns the restore for both handles.
func rawTerminal(fd int) (func(), error) {
	in := windows.Handle(fd)
	var oldIn uint32
	if err := windows.GetConsoleMode(in, &oldIn); err != nil {
		return nil, err
	}
	rawIn := oldIn &^ uint32(windows.ENABLE_ECHO_INPUT|windows.ENABLE_LINE_INPUT|windows.ENABLE_PROCESSED_INPUT)
	rawIn |= windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	if err := windows.SetConsoleMode(in, rawIn); err != nil {
		return nil, err
	}
	restore := func() { windows.SetConsoleMode(in, oldIn) }

	// Best-effort: stdout may be redirected (no console mode to set), and the
	// input raw mode above is the part attach cannot work without.
	out := windows.Handle(os.Stdout.Fd())
	var oldOut uint32
	if windows.GetConsoleMode(out, &oldOut) == nil {
		newOut := oldOut | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
		if windows.SetConsoleMode(out, newOut) == nil {
			restore = func() {
				windows.SetConsoleMode(in, oldIn)
				windows.SetConsoleMode(out, oldOut)
			}
		}
	}
	return restore, nil
}

// watchResize relays the console's size to the holder by polling: Windows has
// no resize signal. Only changes are sent, so the idle cost is one GetSize per
// tick. Returns a stop.
func watchResize(send func(byte, []byte) bool, fd int) func() {
	if !term.IsTerminal(fd) {
		return func() {}
	}
	stop := make(chan struct{})
	go func() {
		lastH, lastW, _ := terminalSize(fd)
		t := time.NewTicker(resizePoll)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				h, w, ok := terminalSize(fd)
				if !ok || (w == lastW && h == lastH) {
					continue
				}
				lastW, lastH = w, h
				send(MsgResize, EncodeResize(h, w))
			}
		}
	}()
	return func() { close(stop) }
}

func terminalSize(fd int) (rows, cols uint16, ok bool) {
	if r, c, ok := envTerminalSize("AX_COLUMNS", "AX_LINES"); ok {
		return r, c, true
	}
	if r, c, ok := envTerminalSize("COLUMNS", "LINES"); ok {
		return r, c, true
	}
	var bestRows, bestCols int
	consider := func(w, h int, err error) {
		if err != nil || w <= 0 || h <= 0 {
			return
		}
		if w > bestCols {
			bestCols, bestRows = w, h
		}
	}
	consider(term.GetSize(fd))
	consider(term.GetSize(int(os.Stdout.Fd())))
	if bestCols <= 0 || bestRows <= 0 {
		return 0, 0, false
	}
	return uint16(bestRows), uint16(bestCols), true
}

func envTerminalSize(colsName, rowsName string) (rows, cols uint16, ok bool) {
	c, err := strconv.Atoi(os.Getenv(colsName))
	if err != nil || c <= 0 || c > 1000 {
		return 0, 0, false
	}
	r, err := strconv.Atoi(os.Getenv(rowsName))
	if err != nil || r <= 0 || r > 1000 {
		r = 24
	}
	return uint16(r), uint16(c), true
}

// notifyDetach fires detach on the events that mean the viewer is going away:
// Go delivers SIGTERM for a closing console window (CTRL_CLOSE_EVENT) and
// os.Interrupt for Ctrl-Break; Ctrl-C itself arrives as a raw 0x03 byte, not
// a signal, because rawTerminal turned processed input off.
func notifyDetach(detach func()) func() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)
	go func() {
		for range sig {
			detach()
		}
	}()
	return func() { signal.Stop(sig) }
}

// cleanStale is a no-op on Windows: a named pipe cannot go stale, its name
// vanishes with the holder's last handle.
func cleanStale(id string, err error) {}
