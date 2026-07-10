package hold

import (
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// inputRecorder is the minimal "harness pty" behind the holder: it records
// every INPUT byte the server forwards.
type inputRecorder struct {
	mu  sync.Mutex
	got []byte
}

func (r *inputRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, p...)
	return len(p), nil
}

func (r *inputRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.got)
}

// TestSendInputControlConn proves the control-connection contract SendInput
// (`ax send` on the Windows process backend) relies on: the input bytes land
// on the harness pty verbatim, and the zero-size ATTACH triggers neither a
// resize nor a repaint nudge, so an attached viewer's screen is undisturbed.
func TestSendInputControlConn(t *testing.T) {
	if runtime.GOOS != "windows" {
		dir, err := os.MkdirTemp("/tmp", "axhold")
		if err != nil {
			t.Fatalf("tempdir: %v", err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		t.Setenv("XDG_RUNTIME_DIR", dir) // keep the socket path short
	}

	rec := &inputRecorder{}
	var resizes, nudges atomic.Int32
	srv, err := Serve("input-control-test", ServeOpts{
		Input:  rec,
		Resize: func(rows, cols uint16) { resizes.Add(1) },
		Nudge:  func() { nudges.Add(1) },
		Rows:   40,
		Cols:   120,
	})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)

	if err := SendInput("input-control-test", []byte("hello\r")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for rec.String() != "hello\r" {
		if time.Now().After(deadline) {
			t.Fatalf("input not forwarded: got %q", rec.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := resizes.Load(); n != 0 {
		t.Errorf("control connection triggered %d resize(s)", n)
	}
	if n := nudges.Load(); n != 0 {
		t.Errorf("control connection triggered %d nudge(s)", n)
	}
}
