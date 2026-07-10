package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProfileHashStableAndSensitive: the hash is deterministic for equal
// profiles and differs when a profile field changes, so status can treat a hash
// match as an exact in-sync signal.
func TestProfileHashStableAndSensitive(t *testing.T) {
	a := Profile{DefaultHarness: "claude", MuxPrefix: "ax:"}
	b := Profile{DefaultHarness: "claude", MuxPrefix: "ax:"}
	if ProfileHash(a) != ProfileHash(b) {
		t.Fatal("equal profiles must hash equal")
	}
	c := Profile{DefaultHarness: "pi", MuxPrefix: "ax:"}
	if ProfileHash(a) == ProfileHash(c) {
		t.Fatal("a changed field must change the hash")
	}
	if ProfileHash(a) == "" {
		t.Fatal("hash of a valid profile must be non-empty")
	}
}

// TestLatestBackupPicksNewest writes several config.toml.bak.<ts> files and
// asserts LatestBackup returns the one with the largest numeric timestamp, not
// the lexically largest (widths differ on purpose).
func TestLatestBackupPicksNewest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("AX_CONFIG", path)
	if err := os.WriteFile(path, []byte("default_harness = \"claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 999 is lexically largest but numerically smallest; 1000 is the newest.
	for _, ts := range []string{"999", "1000", "200"} {
		if err := os.WriteFile(path+".bak."+ts, []byte("ts "+ts), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A non-numeric-suffixed file must be ignored, not chosen.
	if err := os.WriteFile(path+".bak.keepme", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ts, ok := LatestBackup()
	if !ok {
		t.Fatal("expected a backup to be found")
	}
	if ts != 1000 || filepath.Base(got) != "config.toml.bak.1000" {
		t.Fatalf("newest backup should be .bak.1000, got %s (ts %d)", got, ts)
	}
}

// TestLatestBackupNoneWhenAbsent: with no backups next to the config, LatestBackup
// reports none (so rollback can no-op and change nothing).
func TestLatestBackupNoneWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("AX_CONFIG", path)
	if err := os.WriteFile(path, []byte("default_harness = \"claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := LatestBackup(); ok {
		t.Fatal("no backup should be found when none exist")
	}
}

// TestRestoreBackupAtomicallyReplaces: RestoreBackup writes the backup's bytes
// over the live config in place.
func TestRestoreBackupAtomicallyReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("AX_CONFIG", path)
	if err := os.WriteFile(path, []byte("default_harness = \"pi\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak.500"
	want := "default_harness = \"claude\"\n"
	if err := os.WriteFile(backup, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreBackup(backup); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("config not restored from backup: got %q want %q", got, want)
	}
}

// TestApplyThenRollbackRoundTrips is an end-to-end recovery: apply a profile
// (which snapshots a backup), then restore the newest backup and confirm the
// original bytes come back.
func TestApplyThenRollbackRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("AX_CONFIG", path)
	original := "default_harness = \"pi\"\nmux_prefix = \"old:\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	// Apply a profile that changes mux_prefix; this snapshots original as a backup.
	backup, err := ApplyProfileToFile(Profile{MuxPrefix: "new:"}, 1234567890)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if backup == "" {
		t.Fatal("apply should have written a backup")
	}
	// The live config now differs from original.
	if cur, _ := os.ReadFile(path); string(cur) == original {
		t.Fatal("apply did not change the config")
	}
	// Roll back: newest backup restored over the config.
	found, _, ok := LatestBackup()
	if !ok {
		t.Fatal("backup missing after apply")
	}
	if err := RestoreBackup(found); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if cur, _ := os.ReadFile(path); string(cur) != original {
		t.Fatalf("rollback did not restore the original config:\n got %q\nwant %q", cur, original)
	}
}
