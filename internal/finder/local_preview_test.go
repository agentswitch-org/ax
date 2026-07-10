package finder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

func localPreviewPicker(t *testing.T, s session.Session) *picker {
	t.Helper()
	p := newTestPicker([]session.Session{s}, map[string]view.RowMeta{})
	p.cfg = config.Config{Columns: []string{"status", "name", "title"}, Harnesses: []config.Harness{{Name: s.Harness, Format: "opencode"}}}
	p.previewCache = map[string][]string{}
	p.previewRev = map[string]previewRevision{}
	p.fetching = map[string]bool{}
	p.previewReady = make(chan previewResult, 4)
	return p
}

func TestReindexHoldsUnchangedLocalPreview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preview.txt")
	if err := os.WriteFile(path, []byte("cached body"), 0o600); err != nil {
		t.Fatal(err)
	}
	last := time.Unix(100, 0)
	s := session.Session{ID: "l1", Harness: "open", File: path, Last: last}
	p := localPreviewPicker(t, s)

	key := session.Key(s)
	p.previewCache[key] = []string{"cached body"}
	p.previewRev[key] = previewRevisionFor(s)

	p.applyReindex(reindexResult{sessions: []session.Session{s}, cfg: p.cfg})

	if p.fetching[key] {
		t.Fatalf("unchanged local session must not kick a background preview render")
	}
	if got := strings.Join(p.previewCache[key], "\n"); got != "cached body" {
		t.Fatalf("unchanged local preview cache = %q, want cached body", got)
	}
}

func TestReindexChangedLocalPreviewRendersAsync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preview.txt")
	if err := os.WriteFile(path, []byte("old body"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := session.Session{ID: "l1", Harness: "open", File: path, Last: time.Unix(100, 0)}
	p := localPreviewPicker(t, old)

	key := session.Key(old)
	p.previewCache[key] = []string{"old body"}
	p.previewRev[key] = previewRevisionFor(old)

	if err := os.WriteFile(path, []byte("fresh body"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := old
	fresh.Last = time.Unix(200, 0)
	p.applyReindex(reindexResult{sessions: []session.Session{fresh}, cfg: p.cfg})

	if got := strings.Join(p.previewCache[key], "\n"); got != "old body" {
		t.Fatalf("changed local preview should keep cached body until async render lands, got %q", got)
	}
	if !p.fetching[key] {
		t.Fatalf("changed local session should kick a background preview render")
	}

	select {
	case pr := <-p.previewReady:
		if !pr.local {
			t.Fatalf("local preview result should be marked local")
		}
		if got := strings.Join(pr.lines, "\n"); got != "fresh body" {
			t.Fatalf("async local preview = %q, want fresh body", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("background local preview render never delivered on previewReady")
	}
}
