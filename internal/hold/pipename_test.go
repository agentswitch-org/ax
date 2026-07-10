package hold

import (
	"path/filepath"
	"strings"
	"testing"
)

// The Windows pipe name and the unix socket filename must share the same
// session-id hash: an attach by id resolves to the same holder endpoint on
// either platform, and Sock stays the stable cross-platform key callers use.
func TestPipeNameMirrorsSock(t *testing.T) {
	id := "worker-abc123"
	pipe := pipeName(id)

	const prefix = `\\.\pipe\ax-`
	if !strings.HasPrefix(pipe, prefix) {
		t.Fatalf("pipeName(%q) = %q, want prefix %q", id, pipe, prefix)
	}
	hash := strings.TrimPrefix(pipe, prefix)
	if len(hash) != 16 {
		t.Fatalf("pipe hash %q is %d chars, want 16 (sha1[:8] hex)", hash, len(hash))
	}
	for _, r := range hash {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("pipe hash %q is not lowercase hex", hash)
		}
	}
	if got := filepath.Base(Sock(id)); got != "ax-"+hash+".sock" {
		t.Fatalf("Sock basename %q does not share the pipe hash %q", got, hash)
	}
}

func TestPipeNameDeterministicAndDistinct(t *testing.T) {
	if pipeName("a") != pipeName("a") {
		t.Fatal("pipeName is not deterministic")
	}
	if pipeName("a") == pipeName("b") {
		t.Fatal("distinct ids share a pipe name")
	}
}
