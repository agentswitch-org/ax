package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/session"
)

// writeText drops a plain-text transcript sidecar and returns its path. The test
// harness uses Format "opencode" so view.TextFile returns the file verbatim (no
// textcache rebuild), letting the search engine grep exactly this content.
func writeText(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func searchHarness() config.Config {
	return config.Config{Harnesses: []config.Harness{{Name: "test", Format: "opencode"}}}
}

// Ranked search orders matches by hit count (most first) and carries each
// session's metadata plus matched-line snippets, so one call is the whole
// reuse-vs-spawn evidence set.
func TestSearchResultsRankedByHits(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	cfg := searchHarness()

	fewer := writeText(t, dir, "fewer.txt", "the cache subsystem\nunrelated line\nmentions cache once more\n")
	most := writeText(t, dir, "most.txt", "cache\ncache warmth\nreread the cache\nprompt cache ttl\n")
	none := writeText(t, dir, "none.txt", "nothing relevant here\njust prose\n")

	sessions := []session.Session{
		{ID: "s-fewer", Harness: "test", File: fewer, Task: "study caching", Labels: []string{"project=blog"}, CtxTok: 100, CtxWindow: 200000, Last: time.Unix(1000, 0)},
		{ID: "s-most", Harness: "test", File: most, Task: "warm the cache", Labels: []string{"project=blog"}, CtxTok: 50, CtxWindow: 200000, Last: time.Unix(2000, 0)},
		{ID: "s-none", Harness: "test", File: none, Task: "something else", Last: time.Unix(3000, 0)},
	}

	var a App
	got := a.searchResults(cfg, sessions, "cache")

	if len(got) != 2 {
		t.Fatalf("want 2 matches (the non-matching session excluded), got %d: %+v", len(got), got)
	}
	if got[0].ID != "s-most" || got[1].ID != "s-fewer" {
		t.Fatalf("want most-hits-first ordering [s-most, s-fewer], got [%s, %s]", got[0].ID, got[1].ID)
	}
	if got[0].Hits <= got[1].Hits {
		t.Fatalf("top result must have the most hits: %d vs %d", got[0].Hits, got[1].Hits)
	}
	// Metadata rides along, no second `ax list --json` needed.
	if got[0].Task != "warm the cache" || got[0].Project != "blog" || got[0].CtxWindow != 200000 {
		t.Fatalf("result missing session metadata: %+v", got[0])
	}
	if len(got[0].Snippets) == 0 {
		t.Fatalf("a match must carry snippet lines, got none")
	}
}

// Recency breaks a hit-count tie, so equally-relevant sessions rank freshest-first.
func TestSearchResultsRecencyTiebreak(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	cfg := searchHarness()

	older := writeText(t, dir, "older.txt", "token budget\n")
	newer := writeText(t, dir, "newer.txt", "token budget\n")

	sessions := []session.Session{
		{ID: "older", Harness: "test", File: older, Last: time.Unix(1000, 0)},
		{ID: "newer", Harness: "test", File: newer, Last: time.Unix(9000, 0)},
	}
	var a App
	got := a.searchResults(cfg, sessions, "token budget")
	if len(got) != 2 || got[0].Hits != got[1].Hits {
		t.Fatalf("setup expects two equal-hit matches, got %+v", got)
	}
	if got[0].ID != "newer" {
		t.Fatalf("recency must break the tie freshest-first, got %s first", got[0].ID)
	}
}

// A remote row is not searched locally (its sidecar lives on its owner), and an
// empty query matches nothing.
func TestSearchResultsSkipsRemoteAndEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	cfg := searchHarness()
	f := writeText(t, dir, "r.txt", "cache cache\n")
	sessions := []session.Session{{ID: "r", Host: "box", Harness: "test", File: f}}

	var a App
	if got := a.searchResults(cfg, sessions, "cache"); len(got) != 0 {
		t.Fatalf("remote rows must not be searched locally, got %+v", got)
	}
	if got := a.searchResults(cfg, sessions, "   "); len(got) != 0 {
		t.Fatalf("empty query must match nothing, got %+v", got)
	}
}

// The --json envelope is documented and stable: a "results" array of rich
// objects plus a ranked "ids" list for older/id-only consumers, with the
// documented field names present.
func TestSearchJSONShape(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	cfg := searchHarness()
	f := writeText(t, dir, "a.txt", "cache cache cache\n")
	sessions := []session.Session{
		{ID: "a", Harness: "test", File: f, Task: "t", Last: time.Unix(1, 0), CtxTok: 1, CtxWindow: 2},
	}

	var a App
	results := a.searchResults(cfg, sessions, "cache")
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.ID
	}
	blob, err := json.Marshal(struct {
		Results []SearchResult `json:"results"`
		IDs     []string       `json:"ids"`
	}{results, ids})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]json.RawMessage
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back["results"]; !ok {
		t.Fatal("json envelope missing top-level results array")
	}
	if _, ok := back["ids"]; !ok {
		t.Fatal("json envelope missing backward-compatible ids list")
	}
	// The documented per-result fields survive a round-trip.
	var one []map[string]json.RawMessage
	json.Unmarshal(back["results"], &one)
	for _, field := range []string{"id", "task", "state", "ctx_tok", "ctx_window", "last", "hits", "snippets"} {
		if _, ok := one[0][field]; !ok {
			t.Fatalf("result object missing documented field %q", field)
		}
	}
}
