package axdir

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStatePathStateAndWriteJSON(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)

	wantPath := filepath.Join(root, "ax", "meta", "s1.json")
	if got := StatePath("meta", "s1.json"); got != wantPath {
		t.Fatalf("StatePath = %q, want %q", got, wantPath)
	}
	if _, err := os.Stat(filepath.Join(root, "ax")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("StatePath created root or returned unexpected stat err: %v", err)
	}

	metaDir := State("meta")
	if metaDir != filepath.Join(root, "ax", "meta") {
		t.Fatalf("State = %q, want meta dir", metaDir)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(root, "ax"))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("state root mode = %v, want 0700", got)
		}
	}

	type payload struct {
		Name string `json:"name"`
		Rank int    `json:"rank"`
	}
	target := filepath.Join(metaDir, "s1.json")
	if err := WriteJSON(target, payload{Name: "alpha", Rank: 7}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var got payload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal written json: %v", err)
	}
	if got.Name != "alpha" || got.Rank != 7 {
		t.Fatalf("written payload = %#v", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("json file mode = %v, want 0600", got)
		}
	}
	entries, err := os.ReadDir(metaDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ax-") {
			t.Fatalf("atomic temp file was left behind: %s", e.Name())
		}
	}
}
