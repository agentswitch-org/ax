package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestCopiedCodexDefaultDiscoversRelocatedSession(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	home := filepath.Join(root, "codex-home")
	t.Setenv("CODEX_HOME", home)
	configPath := filepath.Join(root, "config.toml")
	t.Setenv("AX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("[[harness]]\nname = 'codex'\nglob = '~/.codex/sessions/*/*/*/rollout-*.jsonl'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "sessions", "2026", "10", "12", "rollout-2026-10-12T12-00-00-00000000-0000-0000-0000-000000000abc.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"type":"session_meta","payload":{"id":"relocated-chat","cwd":"/project"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"repair the parser"}]}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Keep discovery's relocated store, but never scan the test runner's history.
	var harnesses []config.Harness
	for _, h := range cfg.Harnesses {
		if h.Name == "codex" {
			if len(h.Stores) == 0 || h.Stores[0].Source != "env:CODEX_HOME" {
				t.Fatalf("relocation was suppressed: %+v", h.Stores)
			}
			h.Stores = h.Stores[:1]
			harnesses = append(harnesses, h)
		}
	}
	cfg.Harnesses = harnesses
	for scan := 0; scan < 2; scan++ {
		got := Index(cfg)
		if len(got) != 1 || got[0].ID != "relocated-chat" || got[0].Title != "repair the parser" || got[0].File != path {
			t.Fatalf("scan %d missed relocated Codex chat: %+v", scan, got)
		}
	}
}

func TestIndexRefreshesChangedSizeAndStoreConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	proj := filepath.Join(root, "projects")
	writeClaude(t, proj, "chat", `C:\Users\alice\src`, "before")
	path := filepath.Join(proj, "chat.jsonl")
	stamp := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	h := config.Harness{Name: "claude", Format: "claude", IDRe: `/(?P<id>[^/]+)\.jsonl$`,
		Stores: []config.Store{{Glob: filepath.Join(proj, "*.jsonl"), Label: "win", Xlate: "win2unix", Root: "/mnt/"}}}
	cfg := config.Config{Harnesses: []config.Harness{h}}
	if got := Index(cfg); len(got) != 1 || got[0].Dir != "/mnt/c/Users/alice/src" {
		t.Fatalf("first scan: %+v", got)
	}
	// A timestamp alone is insufficient on coarse-grained filesystems.
	writeClaude(t, proj, "chat", `C:\Users\alice\src`, "a longer updated title")
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if got := Index(cfg); len(got) != 1 || got[0].Title != "a longer updated title" {
		t.Fatalf("stale size: %+v", got)
	}
	// Reusing the parsed file must not reuse old labels or cwd translation.
	cfg.Harnesses[0].Name = "work-claude"
	cfg.Harnesses[0].Stores[0].Label = ""
	cfg.Harnesses[0].Stores[0].Xlate = ""
	if got := Index(cfg); len(got) != 1 || got[0].Harness != "work-claude" || got[0].Store != "" || got[0].Dir != `C:\Users\alice\src` {
		t.Fatalf("stale store config: %+v", got)
	}
}

func resetIndexCache() {
	loaded.Lock()
	loaded.entries, loaded.mtime = nil, 0
	loaded.Unlock()
}

func TestIndexPrunesMissingFilesAndKeepsUnchangedRows(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	project := filepath.Join(root, "project")
	writeClaude(t, project, "keep", "/project", "keep this chat")
	writeClaude(t, project, "remove", "/project", "old chat")
	cfg := config.Config{Harnesses: []config.Harness{{Name: "claude", Format: "claude", IDRe: `/(?P<id>[^/]+)\.jsonl$`, Glob: config.StringList{filepath.Join(project, "*.jsonl")}}}}
	if got := Index(cfg); len(got) != 2 {
		t.Fatalf("initial scan: %+v", got)
	}
	if err := os.Remove(filepath.Join(project, "remove.jsonl")); err != nil {
		t.Fatal(err)
	}
	for scan := 0; scan < 2; scan++ {
		got := Index(cfg)
		if len(got) != 1 || got[0].ID != "keep" || got[0].Title != "keep this chat" {
			t.Fatalf("scan %d: %+v", scan, got)
		}
	}
	if got := loadIndexCache(); len(got) != 1 {
		t.Fatalf("deleted transcript remains cached: %d entries", len(got))
	}
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
	// A copied local store must also deduplicate, including on the cached scan.
	h.Stores[1].Label = ""
	got = Index(config.Config{Harnesses: []config.Harness{h}})
	if len(got) != 1 || got[0].Title != "primary copy" {
		t.Fatalf("copied local store duplicated the session: %+v", got)
	}
	// Identical IDs in two different harnesses are still distinct sessions.
	other := h
	other.Name = "work-claude"
	got = Index(config.Config{Harnesses: []config.Harness{h, other}})
	if len(got) != 2 || got[0].Harness == got[1].Harness {
		t.Fatalf("deduplication crossed harnesses: %+v", got)
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
