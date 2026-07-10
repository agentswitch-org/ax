package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/meta"
)

// TestIndexReflectsChanges proves the in-process caches (transcript index +
// meta) stay correct: a transcript rewritten with a newer mtime is reparsed,
// and a re-tag is merged in, even though a prior Index warmed both caches.
func TestIndexReflectsChanges(t *testing.T) {
	// Reset package-level index cache so a prior test can't leak in.
	loaded.Lock()
	loaded.entries, loaded.mtime = nil, 0
	loaded.Unlock()

	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	proj := filepath.Join(root, "projects", "-Users-noah-src-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "sess-fresh-0001"
	fp := filepath.Join(proj, id+".jsonl")

	write := func(title string, mt time.Time) {
		rec := fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/Users/noah/src/proj","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":%q}}`+"\n", id, title)
		if err := os.WriteFile(fp, []byte(rec), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(fp, mt, mt)
	}

	cfg := config.Config{Harnesses: []config.Harness{{
		Name: "claude", Glob: filepath.Join(root, "projects", "*", "*.jsonl"),
		IDRe: `/(?P<id>[^/]+)\.jsonl$`, Format: "claude",
	}}}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	write("original title", base)
	if got := findByID(Index(cfg), id); got == nil || got.Title != "original title" {
		t.Fatalf("first Index title = %v, want original title", got)
	}

	// Rewrite the transcript with a newer mtime: must be reparsed, not served
	// from the warm index cache.
	write("edited title", base.Add(time.Hour))
	if got := findByID(Index(cfg), id); got == nil || got.Title != "edited title" {
		t.Fatalf("after edit, Index title = %v, want edited title (stale index cache)", got)
	}

	// A re-tag via the meta sidecar must merge in without touching the transcript.
	archivedAt := base.Add(2 * time.Hour)
	if err := meta.Save(id, meta.Meta{Name: "renamed-worker", Archived: true, ArchivedAt: archivedAt}); err != nil {
		t.Fatal(err)
	}
	if got := findByID(Index(cfg), id); got == nil || got.Name != "renamed-worker" || !got.Archived || !got.ArchivedAt.Equal(archivedAt) {
		t.Fatalf("after re-tag, Index name = %v, want renamed-worker (stale meta cache)", got)
	}

	if err := meta.SetArchived(id, false); err != nil {
		t.Fatal(err)
	}
	if got := findByID(Index(cfg), id); got == nil || got.Archived || !got.ArchivedAt.IsZero() {
		t.Fatalf("after unarchive, Index archived fields = %v, want cleared", got)
	}
}

func findByID(ss []Session, id string) *Session {
	for i := range ss {
		if ss[i].ID == id {
			return &ss[i]
		}
	}
	return nil
}
