package hold

import (
	"fmt"
	"time"
)

// inputWait bounds the control connection's handshake, so a send to a wedged
// holder fails with a message instead of hanging the caller.
const inputWait = 5 * time.Second

// SendInput delivers input bytes to session id's holder over its endpoint as a
// control connection: an ATTACH with a zero size, which the server answers
// with HELLO alone (no backlog replay, no resize or repaint nudge, no OUTPUT
// broadcast), so injecting input never disturbs an attached viewer's screen.
// This is how `ax send` reaches a session on Windows, where the process
// backend has no FIFO: the same pipe and INPUT frames an attach client's
// keystrokes use. The bytes land on the harness pty verbatim.
func SendInput(id string, payload []byte) error {
	conn, err := dial(id)
	if err != nil {
		return fmt.Errorf("no holder answering: %w", err)
	}
	defer conn.Close()
	if err := writeFrameDeadline(conn, MsgAttach, EncodeAttach(0, 0), inputWait); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(inputWait))
	typ, p, err := ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("no HELLO from the holder: %w", err)
	}
	switch typ {
	case MsgHello:
		proto, _, err := DecodeHello(p)
		if err != nil {
			return err
		}
		if proto != Proto {
			return fmt.Errorf("holder speaks protocol %d, this ax speaks %d", proto, Proto)
		}
	case MsgExit:
		return fmt.Errorf("session has exited")
	default:
		return fmt.Errorf("unexpected frame %#x from the holder", typ)
	}
	// The server sends EXIT (never BACKLOG/OUTPUT) to a control connection when
	// the harness is already gone; a live one goes straight to its input loop.
	conn.SetReadDeadline(time.Time{})
	if err := writeFrameDeadline(conn, MsgInput, payload, inputWait); err != nil {
		return err
	}
	return writeFrameDeadline(conn, MsgDetach, nil, inputWait)
}
