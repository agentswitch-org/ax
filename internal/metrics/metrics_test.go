package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/runs"
)

func sample() Snapshot {
	return Snapshot{
		Sessions: []SessionMetric{
			{ID: "s1", Harness: "claude", Model: "sonnet", Cost: 1.5, HasCost: true, InTok: 1000, OutTok: 200, CacheRead: 50, CacheWrite: 10, Duration: 90 * time.Second},
		},
		Runs: []RunMetric{
			{Group: "g1", Outcome: runs.Success, Cost: 2.5, Tokens: runs.Tokens{In: 1000, Out: 200, CacheRead: 50, CacheWrite: 10}, Workers: 2, Duration: 5 * time.Minute},
			{Group: "g2", Outcome: runs.GaveUp, Cost: 0.5, Tokens: runs.Tokens{In: 100, Out: 20}, Workers: 1, Duration: 30 * time.Second},
		},
	}
}

// RenderTable must surface every run's outcome, not just the first, so a
// scan of the output tells you which groups failed.
func TestRenderTableListsOutcomes(t *testing.T) {
	out := RenderTable(sample())
	if !strings.Contains(out, "g1") || !strings.Contains(out, "g2") {
		t.Fatalf("missing a run group in table:\n%s", out)
	}
	if !strings.Contains(out, "OUTCOMES  gave_up=1 success=1") {
		t.Fatalf("outcome counts wrong:\n%s", out)
	}
}

// RenderProm must never emit a series keyed by session id or run group: that
// cardinality grows without bound over ax's lifetime and is exactly what
// Prometheus's own instrumentation guidance warns against for a textfile that
// gets rewritten and re-scraped forever.
func TestRenderPromHasBoundedCardinality(t *testing.T) {
	out := RenderProm(sample())
	if strings.Contains(out, `id="s1"`) {
		t.Fatalf("session id leaked into a prom label:\n%s", out)
	}
	if strings.Contains(out, `group="g1"`) || strings.Contains(out, `group="g2"`) {
		t.Fatalf("run group leaked into a prom label:\n%s", out)
	}
	if !strings.Contains(out, `ax_runs_concluded{outcome="success"} 1`) {
		t.Fatalf("missing success run count:\n%s", out)
	}
	if !strings.Contains(out, `ax_runs_concluded{outcome="gave_up"} 1`) {
		t.Fatalf("missing gave_up run count:\n%s", out)
	}
	if !strings.Contains(out, `ax_run_cost_dollars{outcome="success"} 2.5`) {
		t.Fatalf("wrong success cost sum:\n%s", out)
	}
	if !strings.Contains(out, `ax_session_cost_dollars 1.5`) {
		t.Fatalf("wrong session cost sum:\n%s", out)
	}
}

// WriteTextfile creates the target directory and writes atomically (no
// half-written file a concurrent node_exporter scrape could read).
func TestWriteTextfileCreatesDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ax.prom")
	if err := WriteTextfile(path, sample()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ax_runs_concluded") {
		t.Fatalf("written file missing expected content:\n%s", data)
	}
}

// A session with no price-DB entry (HasCost false) must render as "-", never
// as a fabricated cost, and must not corrupt the prom cost sum. This guards
// the fix for a real bug: view.Cost's -1 "no data" sentinel was once summed
// and printed as a raw dollar figure ($-1.00).
func TestNoPriceDataSessionExcludedFromCost(t *testing.T) {
	snap := Snapshot{Sessions: []SessionMetric{
		{ID: "priced", Cost: 2, HasCost: true},
		{ID: "unpriced", Cost: 0, HasCost: false},
	}}
	table := RenderTable(snap)
	if !strings.Contains(table, "unpriced") {
		t.Fatalf("missing unpriced session row:\n%s", table)
	}
	for _, line := range strings.Split(table, "\n") {
		if strings.HasPrefix(line, "unpriced") && !strings.Contains(line, "-") {
			t.Fatalf("unpriced session should render cost as '-':\n%s", line)
		}
	}
	prom := RenderProm(snap)
	if !strings.Contains(prom, "ax_session_cost_dollars 2\n") {
		t.Fatalf("cost sum should exclude the unpriced session:\n%s", prom)
	}
}

// duration guards against a clock skew or a zero Created/Started producing a
// negative or bogus span.
func TestDurationGuardsBadInputs(t *testing.T) {
	if d := duration(time.Time{}, time.Now()); d != 0 {
		t.Fatalf("zero start should give 0 duration, got %v", d)
	}
	now := time.Now()
	if d := duration(now, now.Add(-time.Minute)); d != 0 {
		t.Fatalf("end before start should give 0 duration, got %v", d)
	}
}
