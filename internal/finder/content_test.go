package finder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/session"
)

type blockedSearch struct {
	started chan string
	release chan struct{}
}

func (s blockedSearch) Matches(query string, files []string, _ int) map[string][]int {
	s.started <- query
	<-s.release
	return map[string][]int{files[0]: {1}}
}

func receiveContent[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("background content operation did not complete")
		var zero T
		return zero
	}
}

func TestSlowContentSearchKeepsTypingLiveAndDropsStaleResults(t *testing.T) {
	p := newTestPicker([]session.Session{{ID: "a", File: "transcript"}}, nil)
	p.mode, p.query = mContent, "old"
	p.done = make(chan struct{})
	defer close(p.done)
	p.searchReady = make(chan searchResult, 1)
	p.ls = time.NewTimer(time.Hour)
	defer p.ls.Stop()
	s := blockedSearch{started: make(chan string, 2), release: make(chan struct{}, 2)}
	p.searcher = s
	p.kickLocalSearch()
	if q := receiveContent(t, s.started); q != "old" {
		t.Fatalf("first query = %q", q)
	}
	// The disk worker is blocked, but editing the query must still return.
	p.handle(ev{t: evRune, r: 'x'})
	if p.query != "oldx" || len(p.matches) != 0 {
		t.Fatalf("typing left stale matches: query=%q matches=%v", p.query, p.matches)
	}
	p.kickLocalSearch()
	select {
	case q := <-s.started:
		t.Fatalf("started overlapping search %q", q)
	default:
	}
	s.release <- struct{}{}
	p.applySearchResult(receiveContent(t, p.searchReady))
	if len(p.matches) != 0 {
		t.Fatal("stale search result became selectable")
	}
	if q := receiveContent(t, s.started); q != "oldx" {
		t.Fatalf("pending search did not use latest query: %q", q)
	}
	// Enter commits a content search; its pending results must still arrive.
	if p.handle(ev{t: evEnter}) {
		t.Fatal("Enter resumed before content search finished")
	}
	s.release <- struct{}{}
	p.applySearchResult(receiveContent(t, p.searchReady))
	if p.localQuery != "oldx" || len(p.matches) != 1 || p.localSearchInFlight {
		t.Fatalf("latest results not applied: query=%q matches=%v inFlight=%v", p.localQuery, p.matches, p.localSearchInFlight)
	}
}

func TestContentPreviewLoadsOffInputLoop(t *testing.T) {
	f := filepath.Join(t.TempDir(), "session.txt")
	if err := os.WriteFile(f, []byte("first\nneedle in a transcript\nlast"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := session.Session{ID: "a", File: f}
	p := newTestPicker([]session.Session{s}, nil)
	p.mode, p.query = mContent, "needle"
	p.contentReady = make(chan contentPreviewResult, 1)
	p.done = make(chan struct{})
	defer close(p.done)
	p.buildContentPreview(s, p.query)
	if !p.contentInFlight || !strings.Contains(strings.Join(p.preview, ""), "loading") {
		t.Fatal("preview must show a placeholder while its background job runs")
	}
	result := receiveContent(t, p.contentReady)
	p.contentInFlight = false
	p.contentPreview = &result
	p.buildContentPreview(s, p.query)
	if len(p.matchLines) != 1 || p.matchLines[0] != 1 || len(p.preview) != 3 {
		t.Fatalf("preview lost content or line numbers: %v %v", p.preview, p.matchLines)
	}
	// Changing the query cannot reuse highlighted lines from the old search.
	p.buildContentPreview(s, "last")
	if !p.contentInFlight || !strings.Contains(strings.Join(p.preview, ""), "loading") {
		t.Fatal("new query reused a stale preview")
	}
	receiveContent(t, p.contentReady)
}
