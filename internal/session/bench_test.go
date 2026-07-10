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

// buildSynthSet lays down n synthetic claude transcripts (plus a meta sidecar
// for each) under a temp XDG_STATE_HOME and returns a config whose glob matches
// them. It models the steady-state list/picker-refresh scenario: a large set of
// unchanging transcripts served from the warm index cache.
func buildSynthSet(tb testing.TB, n int) config.Config {
	tb.Helper()
	root := tb.TempDir()
	os.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	tb.Cleanup(func() { os.Unsetenv("XDG_STATE_HOME") })

	projRoot := filepath.Join(root, "projects")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("sess-%06d-aaaa-bbbb-cccc-000000000000", i)
		proj := filepath.Join(projRoot, fmt.Sprintf("-Users-noah-src-proj%03d", i%50))
		if err := os.MkdirAll(proj, 0o755); err != nil {
			tb.Fatal(err)
		}
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		// A handful of records: session line, a few user/assistant turns with usage.
		var b []byte
		b = append(b, []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"user","content":"do a big refactor of the parser number %d"}}`+"\n", id, "/Users/noah/src/proj", ts, i))...)
		for t := 0; t < 6; t++ {
			mts := base.Add(time.Duration(i)*time.Minute + time.Duration(t)*time.Second).Format(time.RFC3339)
			b = append(b, []byte(fmt.Sprintf(`{"type":"assistant","sessionId":%q,"timestamp":%q,"message":{"id":"msg-%d-%d","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"working on it"}],"usage":{"input_tokens":1200,"output_tokens":800,"cache_read_input_tokens":40000,"cache_creation_input_tokens":2000}}}`+"\n", id, mts, i, t))...)
		}
		fp := filepath.Join(proj, id+".jsonl")
		if err := os.WriteFile(fp, b, 0o644); err != nil {
			tb.Fatal(err)
		}
		// Backdate mtime so it is stable across runs.
		mt := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(fp, mt, mt)

		// A meta sidecar for every session (control-layer metadata merge path).
		meta.Save(id, meta.Meta{
			Name:   fmt.Sprintf("worker-%d", i),
			Task:   "refactor the parser",
			Group:  fmt.Sprintf("run%03d", i%20),
			Origin: "agent",
			Mode:   "interactive",
		})
	}

	return config.Config{Harnesses: []config.Harness{{
		Name:   "claude",
		Glob:   filepath.Join(projRoot, "*", "*.jsonl"),
		IDRe:   `/(?P<id>[^/]+)\.jsonl$`,
		Format: "claude",
	}}}
}

func benchIndex(b *testing.B, n int) {
	cfg := buildSynthSet(b, n)
	// Warm the cache so we measure the steady-state refresh, not the cold parse.
	_ = Index(cfg)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Index(cfg)
	}
}

func BenchmarkIndexWarm100(b *testing.B)  { benchIndex(b, 100) }
func BenchmarkIndexWarm500(b *testing.B)  { benchIndex(b, 500) }
func BenchmarkIndexWarm2000(b *testing.B) { benchIndex(b, 2000) }

// TestIndexTiming prints wall-clock per warm Index call at several scales, so a
// non-benchmark run shows the growth curve directly.
func TestIndexTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	for _, n := range []int{100, 500, 1000, 2000} {
		cfg := buildSynthSet(t, n)
		_ = Index(cfg) // warm
		const reps = 20
		start := time.Now()
		for i := 0; i < reps; i++ {
			_ = Index(cfg)
		}
		per := time.Since(start) / reps
		t.Logf("n=%-5d warm Index: %v/call", n, per)
	}
}
