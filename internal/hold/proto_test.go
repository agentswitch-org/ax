package hold

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"time"
)

// The frame codec is the wire contract between a holder and every future
// client, so a round trip must be exact for every type and payload size,
// including empty and large payloads.
func TestFrameRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		typ     byte
		payload []byte
	}{
		{"empty detach", MsgDetach, nil},
		{"input bytes", MsgInput, []byte("ls -la\r")},
		{"binary output", MsgOutput, []byte{0x1b, '[', '2', 'J', 0x00, 0xff, 0x7f}},
		{"large backlog", MsgBacklog, bytes.Repeat([]byte("x"), 300*1024)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, c.typ, c.payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			typ, payload, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if typ != c.typ {
				t.Errorf("type = %#x, want %#x", typ, c.typ)
			}
			if !bytes.Equal(payload, c.payload) {
				t.Errorf("payload mismatch: got %d bytes, want %d", len(payload), len(c.payload))
			}
		})
	}
}

// Consecutive frames on one stream must parse independently: the reader takes
// exactly one frame's bytes and leaves the rest.
func TestFrameStream(t *testing.T) {
	var buf bytes.Buffer
	WriteFrame(&buf, MsgHello, EncodeHello(1234))
	WriteFrame(&buf, MsgBacklog, []byte("screen"))
	WriteFrame(&buf, MsgOutput, []byte("more"))
	want := []struct {
		typ     byte
		payload string
	}{
		{MsgHello, string(EncodeHello(1234))},
		{MsgBacklog, "screen"},
		{MsgOutput, "more"},
	}
	for i, w := range want {
		typ, payload, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if typ != w.typ || string(payload) != w.payload {
			t.Errorf("frame %d = (%#x, %q), want (%#x, %q)", i, typ, payload, w.typ, w.payload)
		}
	}
}

// A truncated stream (holder died mid-frame) must error, never return a
// partial payload as if complete.
func TestFrameTruncated(t *testing.T) {
	var buf bytes.Buffer
	WriteFrame(&buf, MsgOutput, []byte("hello"))
	whole := buf.Bytes()
	for _, cut := range []int{0, 1, 4, 7} {
		if _, _, err := ReadFrame(bytes.NewReader(whole[:cut])); err == nil {
			t.Errorf("cut at %d bytes: read succeeded, want an error", cut)
		}
	}
}

// An oversized length prefix (corrupt or hostile peer) must be rejected before
// allocation, not honored.
func TestFrameOversize(t *testing.T) {
	hdr := make([]byte, 5)
	hdr[0] = MsgOutput
	binary.LittleEndian.PutUint32(hdr[1:5], maxFrame+1)
	if _, _, err := ReadFrame(bytes.NewReader(hdr)); err == nil || err == io.EOF {
		t.Fatalf("oversize frame accepted, want a limit error (got %v)", err)
	}
}

func TestAttachPayload(t *testing.T) {
	proto, rows, cols, err := DecodeAttach(EncodeAttach(52, 211))
	if err != nil {
		t.Fatal(err)
	}
	if proto != Proto || rows != 52 || cols != 211 {
		t.Errorf("got (proto %d, %dx%d), want (proto %d, 52x211)", proto, rows, cols, Proto)
	}
	if _, _, _, err := DecodeAttach([]byte{1, 2}); err == nil {
		t.Error("short attach payload accepted")
	}
}

func TestResizePayload(t *testing.T) {
	rows, cols, err := DecodeResize(EncodeResize(24, 80))
	if err != nil {
		t.Fatal(err)
	}
	if rows != 24 || cols != 80 {
		t.Errorf("got %dx%d, want 24x80", rows, cols)
	}
	if _, _, err := DecodeResize(nil); err == nil {
		t.Error("empty resize payload accepted")
	}
}

func TestHelloPayload(t *testing.T) {
	proto, pid, err := DecodeHello(EncodeHello(98765))
	if err != nil {
		t.Fatal(err)
	}
	if proto != Proto || pid != 98765 {
		t.Errorf("got (proto %d, pid %d), want (proto %d, pid 98765)", proto, pid, Proto)
	}
	if _, _, err := DecodeHello([]byte{1}); err == nil {
		t.Error("short hello payload accepted")
	}
}

// EXIT carries a signed code (a -1 must survive), the harness runtime for the
// client's fresh-failure rule, and the raw output tail.
func TestExitPayload(t *testing.T) {
	tail := []byte("error: no such model\r\n")
	code, runtime, gotTail, err := DecodeExit(EncodeExit(-1, 2500*time.Millisecond, tail))
	if err != nil {
		t.Fatal(err)
	}
	if code != -1 {
		t.Errorf("code = %d, want -1", code)
	}
	if runtime != 2500*time.Millisecond {
		t.Errorf("runtime = %v, want 2.5s", runtime)
	}
	if !bytes.Equal(gotTail, tail) {
		t.Errorf("tail = %q, want %q", gotTail, tail)
	}
	if _, _, _, err := DecodeExit(make([]byte, 11)); err == nil {
		t.Error("short exit payload accepted")
	}
}
