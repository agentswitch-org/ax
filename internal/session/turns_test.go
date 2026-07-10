package session

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLastReport pins the final-report extraction: it returns the last assistant
// message that carries actual text, skipping a trailing tool-only assistant turn
// (content is a tool_use block with no text), and empty for a transcript with no
// assistant text. This is what `ax result` prints for an interactive worker.
func TestLastReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	content := `{"type":"user","message":{"content":"do the thing"},"timestamp":"2026-07-02T00:00:00Z"}` + "\n" +
		`{"type":"assistant","message":{"id":"a","model":"claude-opus-4-8","content":"first pass","usage":{"output_tokens":3}},"timestamp":"2026-07-02T00:00:01Z"}` + "\n" +
		`{"type":"user","message":{"content":"keep going"},"timestamp":"2026-07-02T00:00:02Z"}` + "\n" +
		`{"type":"assistant","message":{"id":"b","model":"claude-opus-4-8","content":"the final report, in full","usage":{"output_tokens":6}},"timestamp":"2026-07-02T00:00:03Z"}` + "\n" +
		`{"type":"assistant","message":{"id":"c","model":"claude-opus-4-8","content":[{"type":"tool_use","name":"Bash"}],"usage":{"output_tokens":9}},"timestamp":"2026-07-02T00:00:04Z"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LastReport("claude", path); got != "the final report, in full" {
		t.Fatalf("LastReport = %q, want the last text-bearing assistant message", got)
	}

	// No assistant text at all -> empty.
	only := filepath.Join(t.TempDir(), "u.jsonl")
	os.WriteFile(only, []byte(`{"type":"user","message":{"content":"hi"},"timestamp":"2026-07-02T00:00:00Z"}`+"\n"), 0o600)
	if got := LastReport("claude", only); got != "" {
		t.Fatalf("LastReport with no assistant text = %q, want empty", got)
	}

	// An unreadable transcript is empty, not a panic.
	if got := LastReport("claude", filepath.Join(t.TempDir(), "missing.jsonl")); got != "" {
		t.Fatalf("LastReport on a missing file = %q, want empty", got)
	}
}
