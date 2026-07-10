package shell

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// decodePwshEncoded reverses encodePwshCommand: base64 -> UTF-16LE -> string.
func decodePwshEncoded(t *testing.T, enc string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("odd byte length %d, not UTF-16", len(raw))
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(raw[i*2:])
	}
	return string(utf16.Decode(units))
}

func TestEncodePwshCommandRoundTrip(t *testing.T) {
	// The exact shape that killed the win01 launch: multi-line, with a line
	// that begins with a flag PowerShell's -Command would read as `--`.
	cmd := "ax run child \\\n  --task-file /tmp/t.md \\\n  --unattended"
	enc := encodePwshCommand(cmd)
	if got := decodePwshEncoded(t, enc); got != cmd {
		t.Errorf("round-trip mismatch:\n got %q\nwant %q", got, cmd)
	}
}

func TestEncodePwshCommandDeterministic(t *testing.T) {
	cmd := "--task-file /tmp/x\nSecond-Line 'quoted' \"double\" $env:FOO"
	first := encodePwshCommand(cmd)
	if second := encodePwshCommand(cmd); first != second {
		t.Errorf("non-deterministic: %q != %q", first, second)
	}
	// Known-good vector so a change to the encoding is caught, not silently accepted.
	if enc := encodePwshCommand("A"); enc != "QQA=" {
		t.Errorf(`encodePwshCommand("A") = %q, want "QQA="`, enc)
	}
}
