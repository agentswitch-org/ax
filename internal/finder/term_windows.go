//go:build windows

package finder

import (
	"os"
	"strconv"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

var (
	ttyOutMode   uint32
	ttyOutModeOK bool
)

// openTTY opens the Windows console input device for the picker's raw key
// input. Input only: CONIN$ rejects writes ("the handle is invalid"), so
// rendering goes through openTTYOut's CONOUT$ handle instead.
func openTTY() (*os.File, error) {
	return os.OpenFile("CONIN$", os.O_RDWR, 0)
}

// openTTYOut opens the Windows console output device for full-frame rendering
// and size measurement, and turns on VT processing so the picker's ANSI
// sequences are interpreted (Windows Terminal defaults it on, a classic
// conhost window does not). ENABLE_PROCESSED_OUTPUT is set alongside it because
// VT processing is only effective when processed output is also enabled.
func openTTYOut() (*os.File, error) {
	f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		ttyOutMode = mode
		ttyOutModeOK = true
		prepareTTYOut(f)
	}
	return f, nil
}

func prepareTTYOut(f *os.File) {
	if f == nil || !ttyOutModeOK {
		return
	}
	h := windows.Handle(f.Fd())
	_ = windows.SetConsoleMode(h, ttyOutMode|windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}

func restoreTTYOut(f *os.File) {
	if f == nil || !ttyOutModeOK {
		return
	}
	_ = windows.SetConsoleMode(windows.Handle(f.Fd()), ttyOutMode)
}

func measureTTYSize(out *os.File) (int, int, error) {
	var candidates []terminalSize
	c, r, err := term.GetSize(int(out.Fd()))
	if err == nil {
		candidates = append(candidates, terminalSize{cols: c, rows: r})
	}
	if c, r, e := term.GetSize(int(os.Stdout.Fd())); e == nil {
		candidates = append(candidates, terminalSize{cols: c, rows: r})
	}
	if c, r, ok := envTTYSize("AX_COLUMNS", "AX_LINES"); ok {
		candidates = append(candidates, terminalSize{cols: c, rows: r})
	}
	if c, r, ok := envTTYSize("COLUMNS", "LINES"); ok {
		candidates = append(candidates, terminalSize{cols: c, rows: r})
	}
	if c, r, ok := chooseTerminalSize(candidates...); ok {
		return c, r, nil
	}
	return c, r, err
}

func envTTYSize(colsName, rowsName string) (int, int, bool) {
	cols, err := strconv.Atoi(os.Getenv(colsName))
	if err != nil || cols <= 0 {
		return 0, 0, false
	}
	rows, err := strconv.Atoi(os.Getenv(rowsName))
	if err != nil || rows <= 0 {
		rows = 24
	}
	return cols, rows, true
}

func cancelTTYRead(f *os.File) {
	if f == nil {
		return
	}
	// The shared reader blocks in a CONIN$ ReadConsoleInputW from another
	// goroutine. The primary unblock is a sentinel input record: the pending
	// read returns it immediately and the reader, seeing its stop channel
	// closed, exits. CancelIoEx stays as a backup (it is not a reliable wake
	// for a blocked console read on its own) for the case where the sentinel
	// write itself fails.
	_ = wakeConsoleInput(f)
	_ = windows.CancelIoEx(windows.Handle(f.Fd()), nil)
}

// watchResize re-measures the screen on each console WINDOW_BUFFER_SIZE_EVENT
// (delivered by readEvents via winResize, the Windows SIGWINCH) until the
// screen closes.
func (s *screen) watchResize() {
	go func() {
		for {
			select {
			case <-s.done:
				return
			case <-winResize:
				s.measure()
				select {
				case s.resize <- struct{}{}:
				default:
				}
			}
		}
	}()
}
