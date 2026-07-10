//go:build windows

// Runtime tests for the named-pipe endpoint: these need a real Windows
// machine (winio pipes), not just a windows-tagged build. They mirror the
// unix client_pty_test's server-side coverage headlessly (no console), so
// they run in CI on a Windows box without a terminal.

package hold

import (
	"sync"
	"testing"
	"time"
)

type pipeEcho struct {
	mu  sync.Mutex
	got []byte
}

func (e *pipeEcho) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, p...)
	return len(p), nil
}

func (e *pipeEcho) String() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return string(e.got)
}

func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The whole wire path over a named pipe: listen, probe, dial, the ATTACH
// handshake (HELLO, BACKLOG), INPUT to the harness, OUTPUT fan-out, DETACH
// leaving the holder alive, and a second holder refusing the taken pipe.
func TestPipeServeAttachRoundTrip(t *testing.T) {
	const id = "win-pipe-roundtrip"
	echo := &pipeEcho{}
	srv, err := Serve(id, ServeOpts{Input: echo, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer srv.Close()

	if !Probe(id) {
		t.Fatal("Probe = false with a live holder")
	}
	if _, err := listen(id); err == nil {
		t.Fatal("second listen on a held pipe succeeded")
	}

	conn, err := dial(id)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := WriteFrame(conn, MsgAttach, EncodeAttach(24, 80)); err != nil {
		t.Fatalf("attach frame: %v", err)
	}
	typ, payload, err := ReadFrame(conn)
	if err != nil || typ != MsgHello {
		t.Fatalf("first frame = %#x, %v; want HELLO", typ, err)
	}
	if proto, _, err := DecodeHello(payload); err != nil || proto != Proto {
		t.Fatalf("hello proto = %d, %v; want %d", proto, err, Proto)
	}
	if typ, _, err = ReadFrame(conn); err != nil || typ != MsgBacklog {
		t.Fatalf("second frame = %#x, %v; want BACKLOG", typ, err)
	}

	if err := WriteFrame(conn, MsgInput, []byte("keys")); err != nil {
		t.Fatalf("input frame: %v", err)
	}
	waitForCond(t, "input to reach the harness", func() bool { return echo.String() == "keys" })

	srv.Output([]byte("painted"))
	typ, payload, err = ReadFrame(conn)
	if err != nil || typ != MsgOutput || string(payload) != "painted" {
		t.Fatalf("output frame = %#x %q, %v", typ, payload, err)
	}

	WriteFrame(conn, MsgDetach, nil)
	waitForCond(t, "holder to drop the client", func() bool { return len(srv.clientList()) == 0 })
	if !Probe(id) {
		t.Fatal("holder gone after detach; it must keep running")
	}
}

// EXIT delivery: an attached client gets the harness's exit code and tail,
// and after Close the pipe name is gone (Probe false, dial fails).
func TestPipeExitAndTeardown(t *testing.T) {
	const id = "win-pipe-exit"
	srv, err := Serve(id, ServeOpts{Input: &pipeEcho{}, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer srv.Close()

	conn, err := dial(id)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	WriteFrame(conn, MsgAttach, EncodeAttach(24, 80))
	if typ, _, err := ReadFrame(conn); err != nil || typ != MsgHello {
		t.Fatalf("no HELLO: %#x, %v", typ, err)
	}
	if typ, _, err := ReadFrame(conn); err != nil || typ != MsgBacklog {
		t.Fatalf("no BACKLOG: %#x, %v", typ, err)
	}

	go srv.Exit(3, 42*time.Second, []byte("boom"), false)
	typ, payload, err := ReadFrame(conn)
	if err != nil || typ != MsgExit {
		t.Fatalf("exit frame = %#x, %v", typ, err)
	}
	code, runtime, tail, err := DecodeExit(payload)
	if err != nil || code != 3 || runtime != 42*time.Second || string(tail) != "boom" {
		t.Fatalf("exit = %d %s %q, %v; want 3 42s boom", code, runtime, tail, err)
	}

	srv.Close()
	if Probe(id) {
		t.Fatal("Probe = true after the holder closed")
	}
	if _, err := dial(id); err == nil {
		t.Fatal("dial succeeded after the holder closed")
	}
}

// The adopt alias: a second listener on the real id lands on the same holder.
func TestPipeListenAlso(t *testing.T) {
	const id, alias = "win-pipe-main", "win-pipe-alias"
	srv, err := Serve(id, ServeOpts{Input: &pipeEcho{}, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer srv.Close()
	if err := srv.ListenAlso(alias); err != nil {
		t.Fatalf("ListenAlso: %v", err)
	}
	if !Probe(alias) {
		t.Fatal("alias pipe not answering")
	}
	conn, err := dial(alias)
	if err != nil {
		t.Fatalf("dial alias: %v", err)
	}
	defer conn.Close()
	WriteFrame(conn, MsgAttach, EncodeAttach(24, 80))
	if typ, _, err := ReadFrame(conn); err != nil || typ != MsgHello {
		t.Fatalf("no HELLO on the alias: %#x, %v", typ, err)
	}
}
