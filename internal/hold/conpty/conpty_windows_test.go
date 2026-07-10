//go:build windows

// Runtime tests for the ConPTY wrapper: these need a real Windows machine
// (CreatePseudoConsole), not just a windows-tagged build. They are the
// verification suite for the ConPTY milestone; cross-compilation only
// type-checks them.

package conpty

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStartRejectsEmptyArgv(t *testing.T) {
	if _, err := Start(nil, Options{}); err == nil {
		t.Fatal("Start(nil) succeeded")
	}
	if _, err := Start([]string{""}, Options{}); err == nil {
		t.Fatal("Start with empty argv0 succeeded")
	}
}

// The full lifecycle: spawn cmd.exe under the pseudoconsole, see its output
// arrive on the out pipe, Wait for its exit code, Close, and get EOF on the
// read loop (in that order: ConPTY holds the pipe open across the exit).
func TestEchoLifecycle(t *testing.T) {
	p, err := Start([]string{"cmd", "/c", "echo conpty-ok"}, Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	if p.Pid() <= 0 {
		t.Fatalf("Pid() = %d", p.Pid())
	}

	var mu sync.Mutex
	var out []byte
	eof := make(chan struct{})
	go func() {
		defer close(eof)
		buf := make([]byte, 4096)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				mu.Lock()
				out = append(out, buf[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	waited := make(chan int, 1)
	go func() {
		code, err := p.Wait()
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
		waited <- code
	}()
	select {
	case code := <-waited:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cmd /c echo did not exit")
	}

	// Close tears the console down while the reader above keeps draining
	// (the drain-or-deadlock rule); EOF must follow.
	p.Close()
	select {
	case <-eof:
	case <-time.After(15 * time.Second):
		t.Fatal("no EOF on the output pipe after Close")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(string(out), "conpty-ok") {
		t.Fatalf("output does not contain the echo; got %q", out)
	}
}

// Input reaches the program: a `findstr`-style read loop under cmd echoes the
// line typed at it. set /p reads one line from the console input; /v:on with
// !X! defers the expansion past parse time (%X% would expand before set runs).
func TestInputRoundTrip(t *testing.T) {
	p, err := Start([]string{"cmd", "/q", "/v:on", "/c", "set /p X= && echo got-!X!"}, Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	var mu sync.Mutex
	var out []byte
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				mu.Lock()
				out = append(out, buf[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	if _, err := p.Write([]byte("hello\r")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := strings.Contains(string(out), "got-hello")
		mu.Unlock()
		if ok {
			p.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("typed input never echoed back; output %q", out)
}

func TestResizeAndNudge(t *testing.T) {
	p, err := Start([]string{"cmd", "/c", "pause"}, Options{Rows: 30, Cols: 100})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Resize(40, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	p.Nudge() // must not panic or error the console
	p.Kill()
	if _, err := p.Wait(); err != nil {
		t.Fatalf("Wait after Kill: %v", err)
	}
	p.Close()
	if err := p.Resize(10, 10); err == nil {
		t.Fatal("Resize after Close succeeded")
	}
	p.Close() // idempotent
}
