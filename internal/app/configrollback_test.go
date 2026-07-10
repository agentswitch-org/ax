package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
)

// TestRollbackRestoresLatestBackup: with a live config and a newer backup,
// `config rollback --yes` restores the backup's bytes over the config.
func TestRollbackRestoresLatestBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("AX_CONFIG", path)
	if err := os.WriteFile(path, []byte("default_harness = \"pi\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "default_harness = \"claude\"\n"
	// Two backups; the newer (larger ts) is the one that should be restored.
	if err := os.WriteFile(path+".bak.100", []byte("default_harness = \"old\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak.200", []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"rollback", "--yes"})
	})
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("rollback restored the wrong backup: got %q want %q", got, want)
	}
	if !strings.Contains(out, "config.toml.bak.200") {
		t.Fatalf("rollback should name the restored backup:\n%s", out)
	}
}

// TestRollbackNoBackupChangesNothing: with no backup present, rollback says so
// and leaves the config untouched.
func TestRollbackNoBackupChangesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("AX_CONFIG", path)
	original := "default_harness = \"pi\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"rollback", "--yes"})
	})
	if !strings.Contains(out, "no backup") {
		t.Fatalf("rollback with no backup should say so:\n%s", out)
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("config must be untouched when there is no backup, got %q", got)
	}
}

// TestRollbackNoOpWhenBackupMatches: a backup identical to the current config is
// a no-op (nothing restored, no error).
func TestRollbackNoOpWhenBackupMatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("AX_CONFIG", path)
	same := "default_harness = \"claude\"\n"
	if err := os.WriteFile(path, []byte(same), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak.300", []byte(same), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"rollback", "--yes"})
	})
	if !strings.Contains(out, "already matches") {
		t.Fatalf("an identical backup should be reported as a no-op:\n%s", out)
	}
}

// TestRollbackShowsProfileDiff: rolling back a mux_prefix change prints the
// profile-level diff of what will be reverted.
func TestRollbackShowsProfileDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("AX_CONFIG", path)
	if err := os.WriteFile(path, []byte("mux_prefix = \"new:\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak.400", []byte("mux_prefix = \"old:\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"rollback", "--yes"})
	})
	if !strings.Contains(out, "mux_prefix") || !strings.Contains(out, "old:") {
		t.Fatalf("rollback should diff the reverted profile fields:\n%s", out)
	}
	// And it should really be reverted.
	got, _ := os.ReadFile(path)
	if p, _ := config.ProfileFromBytes(got); p.MuxPrefix != "old:" {
		t.Fatalf("mux_prefix not reverted, got %q", p.MuxPrefix)
	}
}
