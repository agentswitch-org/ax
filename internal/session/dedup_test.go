package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Claude re-logs an assistant message with identical usage; tokens must be
// counted once (summing duplicates inflated cost ~2x).
func TestParseClaudeDedupesUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := `{"type":"assistant","message":{"id":%q,"model":"claude-opus-4-8","usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}` + "\n"
	content := ""
	for i := 0; i < 3; i++ { // same message id 'a' re-logged 3x
		content += fmt.Sprintf(line, "a", 100, 10, 2000, 50)
	}
	content += fmt.Sprintf(line, "b", 5, 3, 4000, 20) // a distinct later turn
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s := parseClaude(path)
	if s == nil {
		t.Fatal("nil session")
	}
	// each id counted once: a(100/10/2000/50) + b(5/3/4000/20)
	if s.InTok != 105 || s.OutTok != 13 || s.CacheReadT != 6000 || s.CacheWriteT != 70 {
		t.Fatalf("deduped totals wrong: in=%d out=%d cr=%d cw=%d (want 105/13/6000/70)",
			s.InTok, s.OutTok, s.CacheReadT, s.CacheWriteT)
	}
	if s.CtxTok != 4025 { // context is the last turn's size (message b): 5+4000+20
		t.Fatalf("ctxTok=%d want 4025", s.CtxTok)
	}
}

// A trailing harness-injected message logs model "<synthetic>" (e.g. the error
// after blowing the context window); the session must keep its last real model,
// or resume rides in with --model '<synthetic>' and dies.
func TestParseClaudeSkipsSyntheticModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	content := `{"type":"assistant","message":{"id":"a","model":"claude-opus-4-8","usage":{}}}` + "\n" +
		`{"type":"assistant","message":{"id":"b","model":"<synthetic>","usage":{}}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s := parseClaude(path)
	if s == nil {
		t.Fatal("nil session")
	}
	if s.Model != "claude-opus-4-8" {
		t.Fatalf("model=%q, want the last real model", s.Model)
	}

	// A transcript with only synthetic messages records no model at all.
	only := filepath.Join(t.TempDir(), "o.jsonl")
	os.WriteFile(only, []byte(`{"type":"assistant","message":{"id":"a","model":"<synthetic>","usage":{}}}`+"\n"), 0o600)
	if s := parseClaude(only); s == nil || s.Model != "" {
		t.Fatalf("synthetic-only model=%q, want empty", s.Model)
	}
}
