package hold

import (
	"net"
	"os"
	"sync"
	"time"

	"github.com/agentswitch-org/ax/internal/axlog"
)

// ServeOpts wires a Server to the pty it fronts. The server knows nothing of
// sessions, meta, or the mux: it holds one program and serves its endpoint.
type ServeOpts struct {
	// Input receives clients' INPUT bytes (the harness pty master).
	Input interface{ Write([]byte) (int, error) }
	// Resize applies a client's terminal size to the pty (TIOCSWINSZ, which
	// makes the kernel deliver SIGWINCH to the harness when the size changed).
	Resize func(rows, cols uint16)
	// Nudge forces a repaint when a reattach size did NOT change (no natural
	// SIGWINCH): kill(-pid, SIGWINCH) on unix.
	Nudge func()
	// Rows/Cols seed the size bookkeeping (the 120x40 a detached run starts at),
	// so a first attach at exactly that size still gets the Nudge.
	Rows, Cols uint16
	// RingSize overrides the output ring capacity (0 = DefaultRingSize).
	RingSize int
}

// attachTimeout bounds the wait for a fresh connection's ATTACH frame, so a
// probe (dial-and-close) or a stuck peer never pins a goroutine.
const attachTimeout = 10 * time.Second

// writeTimeout bounds each client write, so one wedged viewer cannot stall the
// pty read loop; a client that cannot drain in time is dropped (it can reattach).
const writeTimeout = 5 * time.Second

// clientOutputQueue bounds each attached client's pending holder->client frames.
// A client that cannot keep this queue draining is disconnected instead of
// applying backpressure to the holder's pty/ConPTY drain loop.
const clientOutputQueue = 64

// clientEnqueueGrace lets a viewer drain a transient repaint burst before the
// holder declares it wedged and drops the connection.
const clientEnqueueGrace = 250 * time.Millisecond

// inputQueue bounds client INPUT frames waiting for the harness pty. If the
// pty input side wedges, the server reader loop still keeps processing detach
// and connection close events instead of blocking inline in Input.Write.
const inputQueue = 256

// lingerWait is how long an exiting holder with a fresh failure and nobody
// attached waits for a first client, so the launch window that is racing to
// attach still receives the EXIT report instead of a vanished socket.
const lingerWait = 10 * time.Second

// Server is the native holder: the per-session listener living inside `ax run`,
// multiplexing the harness pty to any number of attach clients and keeping the
// output ring for reattach repaint.
type Server struct {
	opts ServeOpts
	ring *Ring
	pid  int

	mu         sync.Mutex
	listeners  []net.Listener
	paths      []string
	clients    map[*holdConn]bool
	rows, cols uint16
	exitFrame  []byte // set once the harness exited; served to a late attach
	closed     bool
	inputq     chan []byte

	delivered   chan struct{} // closed when some client has received EXIT
	deliverOnce sync.Once
}

type outboundFrame struct {
	typ        byte
	payload    []byte
	closeAfter bool
}

// holdConn is one attached client. After the attach handshake, all holder to
// client frames go through out and exactly one writer goroutine; the pty drain
// path never calls conn.Write directly.
type holdConn struct {
	conn      net.Conn
	out       chan outboundFrame
	closed    chan struct{}
	closeOnce sync.Once
}

func newHoldConn(conn net.Conn) *holdConn {
	return &holdConn{
		conn:   conn,
		out:    make(chan outboundFrame, clientOutputQueue),
		closed: make(chan struct{}),
	}
}

func (c *holdConn) send(typ byte, payload []byte) error {
	return writeFrameDeadline(c.conn, typ, payload, writeTimeout)
}

func (c *holdConn) enqueue(typ byte, payload []byte, closeAfter bool) bool {
	select {
	case <-c.closed:
		return false
	default:
	}
	frame := outboundFrame{
		typ:        typ,
		payload:    append([]byte(nil), payload...),
		closeAfter: closeAfter,
	}
	timer := time.NewTimer(clientEnqueueGrace)
	defer timer.Stop()
	select {
	case c.out <- frame:
		return true
	case <-c.closed:
		return false
	case <-timer.C:
		return false
	}
}

func (c *holdConn) startWriter(s *Server) {
	go func() {
		for {
			select {
			case frame := <-c.out:
				if err := writeFrameDeadline(c.conn, frame.typ, frame.payload, writeTimeout); err != nil {
					s.drop(c)
					return
				}
				if frame.typ == MsgExit {
					s.deliverOnce.Do(func() { close(s.delivered) })
				}
				if frame.closeAfter {
					s.drop(c)
					return
				}
			case <-c.closed:
				return
			}
		}
	}()
}

func (c *holdConn) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.conn.Close()
	})
}

// Serve starts the holder for session id: it takes the per-session socket and
// accepts attach clients until Close (or Exit) tears it down.
func Serve(id string, opts ServeOpts) (*Server, error) {
	ln, err := listen(id)
	if err != nil {
		return nil, err
	}
	s := &Server{
		opts:      opts,
		ring:      NewRing(opts.RingSize),
		pid:       os.Getpid(),
		clients:   map[*holdConn]bool{},
		rows:      opts.Rows,
		cols:      opts.Cols,
		delivered: make(chan struct{}),
	}
	if opts.Input != nil {
		s.inputq = make(chan []byte, inputQueue)
		go s.inputLoop()
	}
	s.addListener(ln, Sock(id))
	return s, nil
}

// ListenAlso adds a second endpoint for the same holder: the adopt alias, so
// once discovery finds the harness-minted real id, an attach by either id lands
// on this process. This replaces the old socket symlink (which named pipes
// cannot express).
func (s *Server) ListenAlso(id string) error {
	ln, err := listen(id)
	if err != nil {
		return err
	}
	s.addListener(ln, Sock(id))
	return nil
}

func (s *Server) addListener(ln net.Listener, path string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		os.Remove(path)
		return
	}
	s.listeners = append(s.listeners, ln)
	s.paths = append(s.paths, path)
	s.mu.Unlock()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed: holder teardown
			}
			go s.handle(conn)
		}
	}()
}

// Output tees a chunk of harness pty output into the ring and to every
// attached client. Called from the run wrapper's pty read loop.
func (s *Server) Output(p []byte) {
	s.ring.Write(p)
	for _, c := range s.clientList() {
		if !c.enqueue(MsgOutput, p, false) {
			s.drop(c)
		}
	}
}

// Exit reports the harness's end to every attached client and stops accepting.
// With linger set (a fresh non-zero exit: a launch failure) and nobody
// attached, it waits briefly for the client that is racing to attach, so the
// failure is seen instead of the window erroring on a vanished socket.
func (s *Server) Exit(code int, runtime time.Duration, tail []byte, linger bool) {
	frame := EncodeExit(code, runtime, tail)
	s.mu.Lock()
	s.exitFrame = frame
	clients := make([]*holdConn, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		if !c.enqueue(MsgExit, frame, true) {
			s.drop(c)
		}
	}
	if linger && len(clients) == 0 {
		select {
		case <-s.delivered:
		case <-time.After(lingerWait):
		}
	}
}

// Close tears the holder down: listeners closed, socket files removed, client
// conns dropped. Idempotent.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	listeners, paths := s.listeners, s.paths
	clients := make([]*holdConn, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, ln := range listeners {
		ln.Close()
	}
	for _, p := range paths {
		os.Remove(p)
	}
	for _, c := range clients {
		c.close()
	}
}

func (s *Server) inputLoop() {
	for p := range s.inputq {
		s.opts.Input.Write(p)
	}
}

func (s *Server) enqueueInput(c *holdConn, p []byte) bool {
	if s.inputq == nil {
		return true
	}
	cp := append([]byte(nil), p...)
	select {
	case s.inputq <- cp:
		return true
	default:
		s.drop(c)
		return false
	}
}

func (s *Server) clientList() []*holdConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*holdConn, 0, len(s.clients))
	for c := range s.clients {
		out = append(out, c)
	}
	return out
}

func (s *Server) drop(c *holdConn) {
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	c.close()
}

// handle runs one client connection: the ATTACH handshake (HELLO, BACKLOG,
// size), then the input loop until detach, disconnect, or teardown. An ATTACH
// carrying a zero size is a control connection (hold.SendInput: `ax send` on
// the Windows process backend): input only, so it gets no backlog replay, no
// resize or repaint nudge, and never joins the OUTPUT broadcast set — a
// viewer's screen is untouched by an injection happening beside it.
func (s *Server) handle(conn net.Conn) {
	c := newHoldConn(conn)
	conn.SetReadDeadline(time.Now().Add(attachTimeout))
	typ, payload, err := ReadFrame(conn)
	if err != nil || typ != MsgAttach {
		c.close() // a probe (dial-and-close) or a peer not speaking the protocol
		return
	}
	conn.SetReadDeadline(time.Time{})
	proto, rows, cols, err := DecodeAttach(payload)
	if err != nil {
		c.close()
		return
	}
	control := rows == 0 && cols == 0
	// HELLO goes out even on a version mismatch: the client reads it, sees the
	// foreign proto, and reports the mismatch instead of hanging.
	if c.send(MsgHello, EncodeHello(s.pid)) != nil {
		c.close()
		return
	}
	if proto != Proto {
		axlog.Printf("hold: refused attach speaking proto %d (holder speaks %d)", proto, Proto)
		c.close()
		return
	}
	if !control {
		if c.send(MsgBacklog, s.ring.Snapshot()) != nil {
			c.close()
			return
		}
	}

	s.mu.Lock()
	if s.exitFrame != nil || s.closed {
		frame := s.exitFrame
		s.mu.Unlock()
		if frame != nil && c.send(MsgExit, frame) == nil {
			s.deliverOnce.Do(func() { close(s.delivered) })
		}
		c.close()
		return
	}
	sizeChanged := false
	startWriter := false
	if !control {
		s.clients[c] = true
		startWriter = true
		sizeChanged = rows != s.rows || cols != s.cols
		if sizeChanged {
			s.rows, s.cols = rows, cols
		}
	}
	s.mu.Unlock()
	if startWriter {
		c.startWriter(s)
	}

	// Apply the client's size after the backlog replay. A changed size makes
	// TIOCSWINSZ deliver SIGWINCH natively; an unchanged one gets the forced
	// nudge (dtach's winch redraw) so a full-screen harness still repaints.
	// A control connection carries no size and triggers neither.
	if !control {
		if sizeChanged {
			if s.opts.Resize != nil {
				s.opts.Resize(rows, cols)
			}
		} else if s.opts.Nudge != nil {
			s.opts.Nudge()
		}
	}

	for {
		typ, payload, err := ReadFrame(conn)
		if err != nil {
			s.drop(c) // conn drop means detach: drop the client, keep running
			return
		}
		switch typ {
		case MsgInput:
			if !s.enqueueInput(c, payload) {
				return
			}
		case MsgResize:
			rows, cols, err := DecodeResize(payload)
			if err != nil {
				continue
			}
			s.mu.Lock()
			s.rows, s.cols = rows, cols // last resize wins (ax has one viewer at a time)
			s.mu.Unlock()
			if s.opts.Resize != nil {
				s.opts.Resize(rows, cols)
			}
		case MsgDetach:
			s.drop(c)
			return
		case MsgRedraw:
			if s.opts.Nudge != nil {
				s.opts.Nudge()
			}
		}
	}
}
