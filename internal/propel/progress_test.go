package propel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/meta"
)

func TestFingerprintTracksRepeatedEditsButNotTouchesOrWorkerChurn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %s: %v", out, err)
		}
	}
	git("init")
	file := filepath.Join(dir, "file with spaces.txt")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("base")
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "base")
	write("edit one")
	first := Fingerprint(dir, "")
	write("edit two")
	second := Fingerprint(dir, "")
	if first == second {
		t.Fatal("another edit to a dirty file did not count")
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(file, future, future); err != nil {
		t.Fatal(err)
	}
	if got := Fingerprint(dir, ""); got != second {
		t.Fatal("touch counted as progress")
	}
	if err := meta.Save("new-worker", meta.Meta{Group: "run"}); err != nil {
		t.Fatal(err)
	}
	if got := Fingerprint(dir, ""); got != second {
		t.Fatal("new worker counted as progress")
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Fingerprint(dir, ""); got == second {
		t.Fatal("untracked artifact did not count")
	}
}

func TestWatchedArtifactDefinesProgressOutsideGit(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "artifact.txt")
	before := Fingerprint(dir, "artifact.txt")
	if err := os.WriteFile(file, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := Fingerprint(dir, "artifact.txt")
	if first == before {
		t.Fatal("artifact creation not detected")
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("more discussion"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Fingerprint(dir, "artifact.txt"); got != first {
		t.Fatal("unrelated notes counted")
	}
	if err := os.WriteFile(file, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Fingerprint(dir, "artifact.txt"); got == first {
		t.Fatal("same-size edit not detected")
	}
}
