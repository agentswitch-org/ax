package hold

// The framed wire protocol between the holder (`ax run`) and an attach client,
// framed in BOTH directions (abduco's upgrade over dtach's raw downstream):
//
//	frame: type uint8 | len uint32 (LE) | payload[len]
//
// client -> holder: ATTACH (hello: proto + size), INPUT (keystrokes verbatim),
// RESIZE, DETACH (conn drop means the same), REDRAW (explicit repaint nudge).
// holder -> client: HELLO (version check before anything else), BACKLOG (ring
// replay), OUTPUT (live pty stream), EXIT (harness ended: code + runtime + tail).

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// Proto is the protocol version carried in ATTACH and HELLO. A mismatch means
// the holder was started by a different ax; the client refuses rather than
// garbling a live session.
const Proto = 1

// Client -> holder frame types.
const (
	MsgAttach byte = 0x01
	MsgInput  byte = 0x02
	MsgResize byte = 0x03
	MsgDetach byte = 0x04
	MsgRedraw byte = 0x05
)

// Holder -> client frame types.
const (
	MsgHello   byte = 0x81
	MsgBacklog byte = 0x82
	MsgOutput  byte = 0x83
	MsgExit    byte = 0x84
)

// maxFrame bounds a frame payload so a corrupt or hostile peer cannot make the
// reader allocate unboundedly. Larger than any real payload (the ring caps
// BACKLOG, pty reads cap OUTPUT/INPUT).
const maxFrame = 1 << 20

// WriteFrame writes one frame. The header and payload go in a single Write so
// a concurrent writer on the same conn (guarded by the caller's lock) never
// interleaves mid-frame.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	buf[0] = typ
	binary.LittleEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

func writeFrameDeadline(conn net.Conn, typ byte, payload []byte, timeout time.Duration) error {
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	return WriteFrame(conn, typ, payload)
}

// ReadFrame reads one frame, returning its type and payload.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[1:5])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds the %d limit", n, maxFrame)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// EncodeAttach packs an ATTACH payload: {proto, rows uint16, cols uint16}.
func EncodeAttach(rows, cols uint16) []byte {
	p := make([]byte, 5)
	p[0] = Proto
	binary.LittleEndian.PutUint16(p[1:3], rows)
	binary.LittleEndian.PutUint16(p[3:5], cols)
	return p
}

// DecodeAttach unpacks an ATTACH payload.
func DecodeAttach(p []byte) (proto byte, rows, cols uint16, err error) {
	if len(p) != 5 {
		return 0, 0, 0, fmt.Errorf("attach payload is %d bytes, want 5", len(p))
	}
	return p[0], binary.LittleEndian.Uint16(p[1:3]), binary.LittleEndian.Uint16(p[3:5]), nil
}

// EncodeResize packs a RESIZE payload: {rows uint16, cols uint16}.
func EncodeResize(rows, cols uint16) []byte {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint16(p[0:2], rows)
	binary.LittleEndian.PutUint16(p[2:4], cols)
	return p
}

// DecodeResize unpacks a RESIZE payload.
func DecodeResize(p []byte) (rows, cols uint16, err error) {
	if len(p) != 4 {
		return 0, 0, fmt.Errorf("resize payload is %d bytes, want 4", len(p))
	}
	return binary.LittleEndian.Uint16(p[0:2]), binary.LittleEndian.Uint16(p[2:4]), nil
}

// EncodeHello packs a HELLO payload: {proto, pid uint32}. pid is the holder's,
// so a client (or a debugging human) can find the process behind the socket.
func EncodeHello(pid int) []byte {
	p := make([]byte, 5)
	p[0] = Proto
	binary.LittleEndian.PutUint32(p[1:5], uint32(pid))
	return p
}

// DecodeHello unpacks a HELLO payload.
func DecodeHello(p []byte) (proto byte, pid int, err error) {
	if len(p) != 5 {
		return 0, 0, fmt.Errorf("hello payload is %d bytes, want 5", len(p))
	}
	return p[0], int(binary.LittleEndian.Uint32(p[1:5])), nil
}

// EncodeExit packs an EXIT payload: {code int32, runtime-millis uint64, tail}.
// The runtime lets the client apply the fresh-launch-failure rule (hold the
// terminal open) without knowing when the holder started; tail is the
// harness's last output for that failure report.
func EncodeExit(code int, runtime time.Duration, tail []byte) []byte {
	p := make([]byte, 12+len(tail))
	binary.LittleEndian.PutUint32(p[0:4], uint32(int32(code)))
	binary.LittleEndian.PutUint64(p[4:12], uint64(runtime.Milliseconds()))
	copy(p[12:], tail)
	return p
}

// DecodeExit unpacks an EXIT payload.
func DecodeExit(p []byte) (code int, runtime time.Duration, tail []byte, err error) {
	if len(p) < 12 {
		return 0, 0, nil, fmt.Errorf("exit payload is %d bytes, want >= 12", len(p))
	}
	code = int(int32(binary.LittleEndian.Uint32(p[0:4])))
	runtime = time.Duration(binary.LittleEndian.Uint64(p[4:12])) * time.Millisecond
	return code, runtime, p[12:], nil
}
