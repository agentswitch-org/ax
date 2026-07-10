package hold

import (
	"bytes"
	"net"
	"os"
	"testing"
	"time"
)

func TestOutputDropsBlockedClientWithoutBlocking(t *testing.T) {
	srv := &Server{
		ring:      NewRing(1024),
		pid:       os.Getpid(),
		clients:   map[*holdConn]bool{},
		delivered: make(chan struct{}),
	}

	slow := attachPipeClient(t, srv)
	defer slow.Close()
	waitForServerTest(t, "slow client to attach", func() bool { return len(srv.clientList()) == 1 })

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < clientOutputQueue*2; i++ {
			srv.Output([]byte("blocked-output\n"))
		}
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Server.Output blocked behind a client that stopped reading")
	}
	waitForServerTest(t, "blocked client to be dropped", func() bool { return len(srv.clientList()) == 0 })

	healthy := attachPipeClient(t, srv)
	defer healthy.Close()
	waitForServerTest(t, "healthy client to attach", func() bool { return len(srv.clientList()) == 1 })

	srv.Output([]byte("still-alive"))
	typ, payload := readPipeFrame(t, healthy)
	if typ != MsgOutput || !bytes.Equal(payload, []byte("still-alive")) {
		t.Fatalf("healthy client frame = %#x %q, want OUTPUT %q", typ, payload, "still-alive")
	}
}

func attachPipeClient(t *testing.T, srv *Server) net.Conn {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	go srv.handle(serverConn)

	if err := writeFrameDeadline(clientConn, MsgAttach, EncodeAttach(24, 80), time.Second); err != nil {
		clientConn.Close()
		t.Fatalf("ATTACH: %v", err)
	}
	if typ, _ := readPipeFrame(t, clientConn); typ != MsgHello {
		clientConn.Close()
		t.Fatalf("first frame = %#x, want HELLO", typ)
	}
	if typ, _ := readPipeFrame(t, clientConn); typ != MsgBacklog {
		clientConn.Close()
		t.Fatalf("second frame = %#x, want BACKLOG", typ)
	}
	return clientConn
}

func readPipeFrame(t *testing.T, conn net.Conn) (byte, []byte) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	typ, payload, err := ReadFrame(conn)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	return typ, payload
}

func waitForServerTest(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
