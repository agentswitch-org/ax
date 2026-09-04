package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
)

func writeClaude(t *testing.T, dir, id, cwd, title string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":%q}}`+"\n", id, cwd, title)
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resetIndexCache() {
	loaded.Lock()
	loaded.entries, loaded.mtime = nil, 0
	loaded.Unlock()
}

// A harness with two file stores indexes sessions from both, and tags the
// non-primary store's sessions with its label.
func TestIndexWalksSecondaryStore(t *testing.T) {
	resetIndexCache()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	primary := filepath.Join(root, "primary")
	foreign := filepath.Join(root, "win")
	writeClaude(t, filepath.Join(primary, "-p"), "id-local", "/p", "local one")
	writeClaude(t, filepath.Join(foreign, "-w"), "id-win", "/w", "windows one")

	h := config.Harness{
		Name: "claude", Format: "claude",
		IDRe: `/(?P<id>[^/]+)\.jsonl$`,
		Stores: []config.Store{
			{Glob: filepath.Join(primary, "*", "*.jsonl"), Primary: true, Source: "config"},
			{Glob: filepath.Join(foreign, "*", "*.jsonl"), Label: "win", Source: "wsl-auto"},
		},
	}
	got := Index(config.Config{Harnesses: []config.Harness{h}})
	byID := map[string]Session{}
	for _, s := range got {
		byID[s.ID] = s
	}
	if len(byID) != 2 {
		t.Fatalf("indexed %d sessions, want 2: %+v", len(byID), got)
	}
	if byID["id-local"].Store != "" {
		t.Errorf("local session tagged %q, want empty", byID["id-local"].Store)
	}
	if byID["id-win"].Store != "win" {
		t.Errorf("foreign session tagged %q, want win", byID["id-win"].Store)
	}
}

// The same id present in both a primary and a foreign store resolves to the
// primary copy; the foreign one never shadows it.
func TestIndexPrimaryWinsOverForeign(t *testing.T) {
	resetIndexCache()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	primary := filepath.Join(root, "primary")
	foreign := filepath.Join(root, "win")
	writeClaude(t, filepath.Join(primary, "-p"), "dup", "/p", "primary copy")
	writeClaude(t, filepath.Join(foreign, "-w"), "dup", "/w", "foreign copy")

	h := config.Harness{
		Name: "claude", Format: "claude",
		IDRe: `/(?P<id>[^/]+)\.jsonl$`,
		Stores: []config.Store{
			{Glob: filepath.Join(primary, "*", "*.jsonl"), Primary: true, Source: "config"},
			{Glob: filepath.Join(foreign, "*", "*.jsonl"), Label: "win", Source: "wsl-auto"},
		},
	}
	got := Index(config.Config{Harnesses: []config.Harness{h}})
	if len(got) != 1 {
		t.Fatalf("indexed %d, want 1 (dedupe): %+v", len(got), got)
	}
	if got[0].Store != "" || got[0].Title != "primary copy" {
		t.Fatalf("primary copy must win: %+v", got[0])
	}
}

// A win-labeled store translates a recorded Windows cwd into its WSL mount form
// at index time, so DirExists and preview work.
func TestIndexTranslatesForeignCwd(t *testing.T) {
	resetIndexCache()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	foreign := filepath.Join(root, "win")
	writeClaude(t, filepath.Join(foreign, "-w"), "id-x", `C:\Users\noahi\src`, "windows")

	h := config.Harness{
		Name: "claude", Format: "claude",
		IDRe: `/(?P<id>[^/]+)\.jsonl$`,
		Stores: []config.Store{
			{Glob: filepath.Join(foreign, "*", "*.jsonl"), Primary: true, Label: "win", Source: "wsl-auto", Xlate: "win2unix", Root: "/mnt/"},
		},
	}
	got := Index(config.Config{Harnesses: []config.Harness{h}})
	if len(got) != 1 {
		t.Fatalf("indexed %d, want 1", len(got))
	}
	if got[0].Dir != "/mnt/c/Users/noahi/src" {
		t.Fatalf("cwd not translated: %q", got[0].Dir)
	}
}
