package finder

import (
	"testing"
	"time"
)

// A reader that never signals done must not pin teardown: waitReaderExit gives
// up after inputShutdownTimeout, so shutdownInput (and with it exec handoff and
// keybinding suspends) can never freeze on a read the platform failed to abort.
func TestWaitReaderExitBounded(t *testing.T) {
	start := time.Now()
	if waitReaderExit(make(chan struct{})) {
		t.Fatal("waitReaderExit reported exit for a reader that never signaled")
	}
	if waited := time.Since(start); waited > 10*inputShutdownTimeout {
		t.Fatalf("waitReaderExit gave up after %v; bound is %v", waited, inputShutdownTimeout)
	}

	exited := make(chan struct{})
	close(exited)
	if !waitReaderExit(exited) {
		t.Fatal("waitReaderExit missed an already-exited reader")
	}
}
