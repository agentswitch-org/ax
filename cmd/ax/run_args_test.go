package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseRunArgsReadsAndRemovesCommandFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cmd.txt")
	want := "claude --append-system-prompt 'long behavior' task"
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := parseRunArgs([]string{"--hold", "--adopt", "codex", "--cmd-file", path, "sid"})
	if err != nil {
		t.Fatalf("parseRunArgs() error = %v", err)
	}
	if got.id != "sid" || got.command != want || got.adopt != "codex" || !got.holdFail {
		t.Fatalf("parseRunArgs() = %+v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("command file still exists after parse: %v", err)
	}
}

func TestParseRunArgsInlineCommand(t *testing.T) {
	got, err := parseRunArgs([]string{"--hold", "sid", "echo ok"})
	if err != nil {
		t.Fatalf("parseRunArgs() error = %v", err)
	}
	if got.id != "sid" || got.command != "echo ok" || !got.holdFail {
		t.Fatalf("parseRunArgs() = %+v", got)
	}
}
