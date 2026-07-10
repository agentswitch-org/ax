package textcache

import (
	"os"
	"testing"
	"time"
)

func TestEnsureBuildsReusesFreshCacheAndRebuildsStale(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	transcript := writeTranscript(t, `{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"world"}]}}
`)

	cache := Ensure("claude", transcript)
	if cache == "" {
		t.Fatal("Ensure returned empty cache path")
	}
	assertFile(t, cache, "hello\nworld\n")

	if err := os.WriteFile(cache, []byte("cached\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	newer := old.Add(time.Hour)
	if err := os.Chtimes(transcript, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cache, newer, newer); err != nil {
		t.Fatal(err)
	}
	if got := Ensure("claude", transcript); got != cache {
		t.Fatalf("Ensure fresh cache path = %q, want %q", got, cache)
	}
	assertFile(t, cache, "cached\n")

	if err := os.WriteFile(transcript, []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"changed"}]}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(transcript, future, future); err != nil {
		t.Fatal(err)
	}
	if got := Ensure("claude", transcript); got != cache {
		t.Fatalf("Ensure rebuilt cache path = %q, want %q", got, cache)
	}
	assertFile(t, cache, "changed\n")
}

func TestEnsureMissingTranscriptReturnsEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := Ensure("claude", "missing.jsonl"); got != "" {
		t.Fatalf("Ensure missing transcript = %q, want empty", got)
	}
}

func writeTranscript(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "transcript-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
