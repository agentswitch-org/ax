//go:build unix

package finder

import (
	"os"

	"golang.org/x/term"
)

// termState is the tty's saved input mode: on unix, x/term's opaque termios
// snapshot.
type termState = term.State

// makeRawInput arms raw mode on the input tty and returns its original mode
// for restoreInput.
func makeRawInput(tty *os.File) (*termState, error) {
	return term.MakeRaw(int(tty.Fd()))
}

// reRawInput re-arms raw mode without capturing a new original (the first
// capture from makeRawInput stays the restore point; re-capturing here would
// snapshot the already-raw mode and leave the terminal raw on exit).
func reRawInput(tty *os.File) error {
	_, err := term.MakeRaw(int(tty.Fd()))
	return err
}

// restoreInput puts the tty back into the mode makeRawInput captured.
func restoreInput(tty *os.File, st *termState) {
	if st != nil {
		term.Restore(int(tty.Fd()), st)
	}
}

// startInputReader starts the platform key reader: on unix the tty delivers a
// VT byte stream, decoded by the shared parser in term.go.
func startInputReader(tty *os.File, out chan ev, stop chan struct{}, done chan struct{}) {
	go decodeLoop(tty, out, stop, done)
}
