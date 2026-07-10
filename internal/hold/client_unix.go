//go:build unix

package hold

import (
	"errors"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// rawTerminal puts the client's terminal into raw mode so keystrokes (and the
// detach chord) arrive byte-by-byte, returning the restore.
func rawTerminal(fd int) (func(), error) {
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	// Ctrl-Q/Ctrl-S must reach the harness as raw bytes, not act as XON/XOFF
	// (and a detach_key rebound to one of them must still work). MakeRaw
	// clears IXON, but belt-and-braces: clear software flow control explicitly
	// (IXOFF too) so neither byte ever does flow control on this tty. The
	// restore uses the pre-raw state.
	clearFlowControl(fd)
	return func() { term.Restore(fd, old) }, nil
}

// clearFlowControl turns software flow control off on fd so Ctrl-Q (XON) and
// Ctrl-S (XOFF) arrive as raw bytes instead of being eaten by the tty driver.
// Best-effort: called right after a successful MakeRaw, which already cleared
// IXON; this also clears IXOFF and guards any platform where it did not take.
func clearFlowControl(fd int) {
	tio, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return
	}
	tio.Iflag &^= unix.IXON | unix.IXOFF
	unix.IoctlSetTermios(fd, ioctlWriteTermios, tio)
}

// watchResize relays the terminal's size to the holder on every SIGWINCH,
// returning a stop.
func watchResize(send func(byte, []byte) bool, fd int) func() {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if w, h, err := term.GetSize(fd); err == nil {
				send(MsgResize, EncodeResize(uint16(h), uint16(w)))
			}
		}
	}()
	return func() { signal.Stop(winch) }
}

// notifyDetach fires detach on the signals that mean the viewer is going away:
// SIGTERM/SIGHUP (window closed, kill), and SIGQUIT because a terminal whose
// raw mode did not take (ISIG still on somewhere in a nested-pty chain) turns
// Ctrl-backslash into SIGQUIT instead of delivering the 0x1c byte, and it is
// the same keystroke meaning the same thing.
func notifyDetach(detach func()) func() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		for range sig {
			detach()
		}
	}()
	return func() { signal.Stop(sig) }
}

// cleanStale removes a stale socket file before spawning a fresh holder: a
// dial refused means the file outlived its holder (died without cleanup),
// dtach -A style.
func cleanStale(id string, err error) {
	if errors.Is(err, syscall.ECONNREFUSED) {
		os.Remove(Sock(id))
	}
}

func terminalSize(fd int) (rows, cols uint16, ok bool) {
	if !term.IsTerminal(fd) {
		return 0, 0, false
	}
	w, h, err := term.GetSize(fd)
	if err != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return uint16(h), uint16(w), true
}
