//go:build unix

package finder

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// openTTY opens the controlling terminal for the picker's raw key input.
func openTTY() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

// openTTYOut opens the controlling terminal for full-frame rendering. It is a
// separate fd from openTTY's so the output side survives suspendInput closing
// the reader's fd (and so the split matches Windows, where input and output
// are genuinely different console devices).
func openTTYOut() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_WRONLY, 0)
}

func prepareTTYOut(*os.File) {}

func restoreTTYOut(*os.File) {}

func cancelTTYRead(*os.File) {}

func measureTTYSize(out *os.File) (int, int, error) {
	return term.GetSize(int(out.Fd()))
}

// watchResize re-measures the screen on each SIGWINCH until the screen closes.
func (s *screen) watchResize() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-s.done:
				signal.Stop(ch)
				return
			case <-ch:
				s.measure()
				select {
				case s.resize <- struct{}{}:
				default:
				}
			}
		}
	}()
}
