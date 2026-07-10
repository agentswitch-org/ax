package hold

import (
	"bytes"
	"testing"
	"time"
)

func TestHoldConnEnqueueWaitsForTransientBacklog(t *testing.T) {
	c := &holdConn{
		out:    make(chan outboundFrame, 1),
		closed: make(chan struct{}),
	}
	c.out <- outboundFrame{typ: MsgOutput, payload: []byte("already queued")}

	go func() {
		time.Sleep(10 * time.Millisecond)
		<-c.out
	}()

	if !c.enqueue(MsgOutput, []byte("after drain"), false) {
		t.Fatal("enqueue returned false for a transiently full queue")
	}
	frame := <-c.out
	if frame.typ != MsgOutput || !bytes.Equal(frame.payload, []byte("after drain")) {
		t.Fatalf("queued frame = (%#x, %q), want output after drain", frame.typ, frame.payload)
	}
}
