package finder

import (
	"testing"
	"time"
)

// decodeString runs the decoder over a fully buffered byte sequence and
// returns the events it emits. The channel is pre-filled and closed, so no
// read inside a sequence ever waits on the escSeqTimeout.
func decodeString(t *testing.T, s string) []ev {
	t.Helper()
	bytes := make(chan byte, len(s))
	for i := 0; i < len(s); i++ {
		bytes <- s[i]
	}
	close(bytes)
	out := make(chan ev, len(s)+8)
	stop := make(chan struct{})
	decodeBytes(bytes, out, stop)
	close(out)
	var evs []ev
	for e := range out {
		evs = append(evs, e)
	}
	return evs
}

func TestDecodeSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []ev
	}{
		{"plain arrows", "\x1b[A\x1b[B\x1b[C\x1b[D", []ev{{t: evUp}, {t: evDown}, {t: evRight}, {t: evLeft}}},
		{"ss3 arrows", "\x1bOA\x1bOD", []ev{{t: evUp}, {t: evLeft}}},
		{"modified arrow leaks nothing", "\x1b[1;5A", []ev{{t: evUp}}},
		{"modified arrow then key", "\x1b[1;2Bq", []ev{{t: evDown}, {t: evRune, r: 'q'}}},
		{"delete drops whole", "\x1b[3~x", []ev{{t: evRune, r: 'x'}}},
		{"tilde navigation keys drop", "\x1b[1~\x1b[4~\x1b[5~\x1b[6~\x1b[7~\x1b[8~", nil},
		{"f-key drops", "\x1b[15~a", []ev{{t: evRune, r: 'a'}}},
		{"HF home end drop", "\x1b[H\x1b[Fq", []ev{{t: evRune, r: 'q'}}},
		{"paste markers drop, body passes", "\x1b[200~hi\r\x1b[201~", []ev{{t: evRune, r: 'h'}, {t: evRune, r: 'i'}, {t: evEnter}}},
		{"sgr mouse press and release drop", "\x1b[<0;12;4M\x1b[<0;12;4mq", []ev{{t: evRune, r: 'q'}}},
		{"x10 mouse eats payload", "\x1b[M !!z", []ev{{t: evRune, r: 'z'}}},
		{"osc drops to bel", "\x1b]0;title\x07k", []ev{{t: evRune, r: 'k'}}},
		{"osc drops to st", "\x1b]0;title\x1b\\k", []ev{{t: evRune, r: 'k'}}},
		{"dcs drops to st", "\x1bPq#0\x1b\\k", []ev{{t: evRune, r: 'k'}}},
		{"alt key is esc plus rune", "\x1bf", []ev{{t: evEsc}, {t: evRune, r: 'f'}}},
		{"plain keys unchanged", "\r\t\x7f\x03\x0ea", []ev{{t: evEnter}, {t: evTab}, {t: evBack}, {t: evCtrlC}, {t: evCtrl, r: 'n'}, {t: evRune, r: 'a'}}},
		{"utf8 rune", "é", []ev{{t: evRune, r: 'é'}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeString(t, tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("event %d: got %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// recvEv reads one event with a deadline so a decoder bug hangs the test with
// a message instead of the go test timeout.
func recvEv(t *testing.T, out <-chan ev) ev {
	t.Helper()
	select {
	case e := <-out:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event within 2s")
		return ev{}
	}
}

func TestDecodeTruncatedCSIDoesNotEatNextKey(t *testing.T) {
	bytes := make(chan byte, 8)
	out := make(chan ev, 8)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		decodeBytes(bytes, out, stop)
	}()
	bytes <- 0x1b
	bytes <- '['
	time.Sleep(3 * escSeqTimeout) // let the sequence read time out
	bytes <- 'q'
	if e := recvEv(t, out); e.t != evRune || e.r != 'q' {
		t.Fatalf("got %v, want rune q", e)
	}
	close(bytes)
	<-done
	if len(out) != 0 {
		t.Fatalf("leaked events: %v", <-out)
	}
}

func TestDecodeLoneEsc(t *testing.T) {
	bytes := make(chan byte, 8)
	out := make(chan ev, 8)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		decodeBytes(bytes, out, stop)
	}()
	bytes <- 0x1b
	if e := recvEv(t, out); e.t != evEsc {
		t.Fatalf("got %v, want esc", e)
	}
	close(bytes)
	<-done
}
