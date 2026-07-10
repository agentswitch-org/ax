package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// writeExtension drops a fake ax-<verb> executable into dir and returns its
// path: a shebang script on unix, a .bat on Windows (exec.LookPath resolves
// the bare "ax-<verb>" name through PATHEXT).
func writeExtension(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		p := filepath.Join(dir, name+".bat")
		if err := os.WriteFile(p, []byte("@echo hi\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtensionEnv(t *testing.T) {
	// AX is appended when absent.
	got := extensionEnv([]string{"PATH=/bin", "HOME=/h"}, "/usr/local/bin/ax")
	if last := got[len(got)-1]; last != "AX=/usr/local/bin/ax" {
		t.Fatalf("want AX appended, got %q", last)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d: %v", len(got), got)
	}

	// An inherited AX is replaced, not duplicated, so a nested `ax foo` whose
	// extension runs `ax bar` does not accumulate stale AX entries.
	got = extensionEnv([]string{"AX=/old/ax", "PATH=/bin"}, "/new/ax")
	n := 0
	for _, kv := range got {
		if kv == "AX=/new/ax" {
			n++
		}
		if kv == "AX=/old/ax" {
			t.Fatalf("stale AX survived: %v", got)
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one AX=/new/ax, got %d: %v", n, got)
	}
}

// TestExtensionLookup checks the PATH lookup ax uses to resolve `ax-<verb>`:
// an executable ax-foo on PATH is found, a non-executable file is not, and an
// unknown verb misses (so the caller falls through to the unknown-command
// error). This exercises the same exec.LookPath("ax-"+cmd) tryExtension uses,
// without the process-replacing exec itself.
func TestExtensionLookup(t *testing.T) {
	dir := t.TempDir()
	exe := writeExtension(t, dir, "ax-foo")
	// Not runnable: no exec bit on unix, no PATHEXT extension on Windows.
	plain := filepath.Join(dir, "ax-bar")
	if err := os.WriteFile(plain, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if got, err := exec.LookPath("ax-foo"); err != nil || got != exe {
		t.Fatalf("want %q found, got %q err=%v", exe, got, err)
	}
	if _, err := exec.LookPath("ax-bar"); err == nil {
		t.Fatal("want non-executable ax-bar to miss")
	}
	if _, err := exec.LookPath("ax-nope"); err == nil {
		t.Fatal("want unknown verb to miss")
	}
}
