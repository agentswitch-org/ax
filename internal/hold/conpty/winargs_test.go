package conpty

import (
	"reflect"
	"testing"
	"unicode/utf16"
)

// A nil env inherits (nil block); an empty one is the empty double-NUL block;
// entries are NUL-separated UTF-16 with a final block terminator.
func TestEnvBlock(t *testing.T) {
	if b, err := envBlock(nil); err != nil || b != nil {
		t.Fatalf("envBlock(nil) = %v, %v; want nil, nil", b, err)
	}
	if b, err := envBlock([]string{}); err != nil || !reflect.DeepEqual(b, []uint16{0, 0}) {
		t.Fatalf("envBlock(empty) = %v, %v; want [0 0], nil", b, err)
	}

	b, err := envBlock([]string{"A=1", "B=two"})
	if err != nil {
		t.Fatalf("envBlock: %v", err)
	}
	want := append(utf16.Encode([]rune("A=1")), 0)
	want = append(want, utf16.Encode([]rune("B=two"))...)
	want = append(want, 0, 0)
	if !reflect.DeepEqual(b, want) {
		t.Fatalf("envBlock layout = %v, want %v", b, want)
	}
}

// Non-BMP values must surrogate-encode, not truncate: the block is UTF-16.
func TestEnvBlockSurrogates(t *testing.T) {
	b, err := envBlock([]string{"E=\U0001F600"})
	if err != nil {
		t.Fatalf("envBlock: %v", err)
	}
	want := append(utf16.Encode([]rune("E=\U0001F600")), 0, 0)
	if !reflect.DeepEqual(b, want) {
		t.Fatalf("envBlock surrogate layout = %v, want %v", b, want)
	}
	if len(want) != 2+2+2 { // "E=", one surrogate pair, entry NUL + block NUL
		t.Fatalf("surrogate pair did not encode as two units: %v", want)
	}
}

func TestEnvBlockRejectsMalformed(t *testing.T) {
	if _, err := envBlock([]string{"NOEQUALS"}); err == nil {
		t.Fatal("entry without '=' accepted")
	}
	if _, err := envBlock([]string{"A=1\x00B=2"}); err == nil {
		t.Fatal("entry with NUL accepted")
	}
}

// COORD fields are int16; protocol sizes are uint16. Clamp, never wrap
// negative (a negative console dimension is rejected by the API).
func TestClampDim(t *testing.T) {
	cases := map[uint16]int16{0: 0, 80: 80, 32767: 32767, 32768: 32767, 65535: 32767}
	for in, want := range cases {
		if got := clampDim(in); got != want {
			t.Fatalf("clampDim(%d) = %d, want %d", in, got, want)
		}
	}
}
