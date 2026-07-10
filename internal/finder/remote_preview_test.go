package finder

import (
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

// A refresh of a remote session that already has a cached preview must not clear
// the cache or drop the pane to the "loading …" placeholder: the cached lines
// have to stay on screen until a fresh fetch replaces them. Dropping the cache
// forces an async ssh round-trip whose loading state flashes on every ~1s
// refresh tick, which is the bug this guards against.
func TestReindexHoldsCachedRemotePreview(t *testing.T) {
	remote := session.Session{ID: "r1", Host: "win01", Title: "remote", Last: time.Unix(100, 0)}
	p := newTestPicker([]session.Session{remote}, map[string]view.RowMeta{})
	p.previewCache = map[string][]string{}
	p.fetching = map[string]bool{}
	p.remotePreview = func(s session.Session) []string { return []string{"fresh remote body"} }

	key := session.Key(remote)
	cached := []string{"cached remote line"}
	p.previewCache[key] = cached

	// A refresh that returns the same remote session, unchanged (Last not advanced).
	p.applyReindex(reindexResult{sessions: []session.Session{remote}, cfg: p.cfg})

	if _, ok := p.previewCache[key]; !ok {
		t.Fatalf("remote preview cache must survive a refresh, but it was cleared")
	}
	if p.fetching[key] {
		t.Fatalf("an unchanged remote session must not trigger a background re-fetch")
	}

	// The next preview build shows the cached body, never the loading placeholder.
	p.buildPreview()
	joined := strings.Join(p.preview, "\n")
	if strings.Contains(joined, "loading preview") {
		t.Fatalf("cached remote preview flashed the loading placeholder: %q", joined)
	}
	if len(p.preview) != 1 || p.preview[0] != "cached remote line" {
		t.Fatalf("cached remote lines should still be shown, got %v", p.preview)
	}
}

// When the remote session actually advanced (its activity timestamp moved), the
// refresh holds the cached body on screen AND kicks a background re-fetch so the
// pane updates seamlessly once the fresh lines land on previewReady.
func TestReindexAdvancedRemoteKicksBackgroundFetch(t *testing.T) {
	old := session.Session{ID: "r1", Host: "win01", Title: "remote", Last: time.Unix(100, 0)}
	p := newTestPicker([]session.Session{old}, map[string]view.RowMeta{})
	p.previewCache = map[string][]string{}
	p.fetching = map[string]bool{}
	p.previewReady = make(chan previewResult, 4)
	p.remotePreview = func(s session.Session) []string { return []string{"fresh remote body"} }

	key := session.Key(old)
	p.previewCache[key] = []string{"cached remote line"}

	// The refresh reports the same session with a newer activity timestamp.
	advanced := old
	advanced.Last = time.Unix(200, 0)
	p.applyReindex(reindexResult{sessions: []session.Session{advanced}, cfg: p.cfg})

	if _, ok := p.previewCache[key]; !ok {
		t.Fatalf("advanced remote refresh must keep the cache until the fetch lands")
	}
	if !p.fetching[key] {
		t.Fatalf("an advanced remote session should kick a background re-fetch")
	}

	// The fresh body arrives on previewReady and overwrites the cache.
	select {
	case pr := <-p.previewReady:
		delete(p.fetching, pr.key)
		p.previewCache[pr.key] = pr.lines
		if len(pr.lines) != 1 || pr.lines[0] != "fresh remote body" {
			t.Fatalf("background fetch should deliver the fresh body, got %v", pr.lines)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("background re-fetch never delivered on previewReady")
	}
}
