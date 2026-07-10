package mux

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/shell"
)

// A short command passes through untouched: no temp file, no-op cleanup, exact
// bytes preserved (the package's rendering must be byte-for-byte on the common
// path).
func TestSpillLongCmdInlineShort(t *testing.T) {
	const cmd = "ax attach abc --cmd 'claude --model sonnet'"
	out, cleanup, err := spillLongCmd(cmd)
	if err != nil {
		t.Fatalf("spillLongCmd: %v", err)
	}
	cleanup()
	if out != cmd {
		t.Fatalf("short command was rewritten: %q", out)
	}
}

// The regression: a launch carrying a large --behavior builds a command far over
// tmux's ~16 KB new-window cap. spillLongCmd must move the body off the command
// line so the window opens on a tiny `sh <file>`, with the full command preserved
// in the script and the script self-deleting after it runs.
func TestSpillLongCmdSpillsAndPreserves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("spill script runs under POSIX sh")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	proof := filepath.Join(tmp, "proof.txt")
	// A >=23 KB payload (the coordinator.md scale that broke the holder), carried
	// as the single-quoted argument of a marker command that records it.
	behavior := strings.Repeat("behavior line with 'quotes' and $vars and `ticks`\n", 500)
	if len(behavior) < 23*1024 {
		t.Fatalf("test payload too small: %d bytes", len(behavior))
	}
	cmd := "printf %s " + shell.QuotePosix(behavior) + " > " + shell.QuotePosix(proof)

	out, cleanup, err := spillLongCmd(cmd)
	if err != nil {
		t.Fatalf("spillLongCmd: %v", err)
	}
	defer cleanup()

	// The window command tmux sees must be short enough to clear the ~16 KB cap.
	if len(out) > spillCmdThreshold {
		t.Fatalf("spilled window command still too long: %d bytes", len(out))
	}
	if !strings.HasPrefix(out, "sh ") {
		t.Fatalf("spilled command is not an sh invocation: %q", out)
	}

	// The spill script exists and holds the whole command plus the self-delete.
	file := strings.TrimSpace(strings.TrimPrefix(out, "sh "))
	file = strings.Trim(file, "'")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read spill script: %v", err)
	}
	if !strings.HasPrefix(string(data), `rm -f "$0"`) {
		t.Fatalf("spill script does not self-delete first: %q", string(data)[:40])
	}
	if !strings.Contains(string(data), cmd) {
		t.Fatal("spill script does not contain the full command")
	}

	// Running the window command delivers the full payload intact, and the script
	// removes itself (Unix keeps the open fd valid past the unlink).
	if err := exec.Command("sh", "-c", out).Run(); err != nil {
		t.Fatalf("run spilled command: %v", err)
	}
	got, err := os.ReadFile(proof)
	if err != nil {
		t.Fatalf("read proof: %v", err)
	}
	if string(got) != behavior {
		t.Fatalf("payload corrupted through spill: got %d bytes, want %d", len(got), len(behavior))
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("spill script was not self-deleted: %v", err)
	}
}
