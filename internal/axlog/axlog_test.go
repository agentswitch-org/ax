package axlog

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestPrintfDumpAndRotate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	Printf("hello %s", "world")
	first := Dump()
	if !bytes.Contains(first, []byte("hello world\n")) {
		t.Fatalf("Dump after Printf = %q, want logged message", first)
	}

	p := Path()
	oversize := bytes.Repeat([]byte("x"), maxSize+1)
	if err := os.WriteFile(p, oversize, 0o600); err != nil {
		t.Fatal(err)
	}
	Printf("after rotation")

	rotated, err := os.ReadFile(p + ".1")
	if err != nil {
		t.Fatalf("read rotated log: %v", err)
	}
	if len(rotated) != len(oversize) {
		t.Fatalf("rotated log size = %d, want %d", len(rotated), len(oversize))
	}
	current, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read current log: %v", err)
	}
	if !strings.Contains(string(current), "after rotation") {
		t.Fatalf("current log = %q, want post-rotation entry", current)
	}
	dump := Dump()
	if !bytes.HasPrefix(dump, oversize[:32]) || !bytes.Contains(dump, []byte("after rotation")) {
		t.Fatal("Dump must concatenate rotated log before current log")
	}
}
