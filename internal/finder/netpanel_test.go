package finder

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/view"
)

// netTestState builds a panel state with the given hosts already answered, so
// the render tests exercise a filled panel (not the querying skeleton).
func netTestState(rows ...view.NetHostStatus) *netPanelState {
	np := &netPanelState{
		rows:     map[string]view.NetHostStatus{},
		querying: map[string]bool{},
	}
	for _, r := range rows {
		np.names = append(np.names, r.Host)
		np.rows[r.Host] = r
	}
	return np
}

func onlineRow(name string, latency int64, sync string) view.NetHostStatus {
	return view.NetHostStatus{
		Host: name, State: view.HostOnline, Reachable: true, LatencyMS: latency,
		AxVersion: "1.4.0", WireVersion: 5, Compat: "ok", OS: "linux", Shell: "sh -c",
		Harnesses: []string{"claude", "codex"}, Sync: sync,
	}
}

// The panel lists every configured host, one row each, and no rendered line
// exceeds the terminal width (the box shrinks to fit rather than overflowing).
func TestNetPanelShowsAllHostsAndFitsWidth(t *testing.T) {
	np := netTestState(
		onlineRow("alpha", 12, "in-sync"),
		onlineRow("bravo", 40, "drift"),
		onlineRow("charlie", 8, "in-sync"),
	)
	for _, cols := range []int{120, 80, 50} {
		p := &picker{sc: &screen{cols: cols, rows: 30}}
		box := p.netPanelBox(np)
		joined := view.StripANSI(strings.Join(box, "\n"))
		for _, h := range []string{"alpha", "bravo", "charlie"} {
			if !strings.Contains(joined, h) {
				t.Fatalf("cols=%d: host %q missing from panel:\n%s", cols, h, joined)
			}
		}
		for _, ln := range box {
			if w := vwidth(ln); w > cols {
				t.Fatalf("cols=%d: line overflows frame (%d > %d): %q", cols, w, cols, view.StripANSI(ln))
			}
		}
	}
}

// An offline host renders gracefully: it still shows a row, marked offline and
// unreachable, and never panics or drops the reachable hosts around it.
func TestNetPanelOfflineHostGraceful(t *testing.T) {
	np := netTestState(
		onlineRow("live", 10, "in-sync"),
		view.NetHostStatus{Host: "dead", State: view.HostOffline, Sync: "unreachable", Compat: "unknown"},
	)
	p := &picker{sc: &screen{cols: 100, rows: 30}}
	joined := view.StripANSI(strings.Join(p.netPanelBox(np), "\n"))
	if !strings.Contains(joined, "dead") || !strings.Contains(joined, "offline") {
		t.Fatalf("offline host should show an offline row:\n%s", joined)
	}
	if !strings.Contains(joined, "live") || !strings.Contains(joined, "in-sync") {
		t.Fatalf("reachable host must survive alongside the offline one:\n%s", joined)
	}
}

// A host still being queried shows a spinner/querying state, not a blank row, so
// the panel is useful the instant it opens (before any ssh round-trip returns).
func TestNetPanelQueryingRowRenders(t *testing.T) {
	np := &netPanelState{
		names:    []string{"pending"},
		rows:     map[string]view.NetHostStatus{},
		querying: map[string]bool{"pending": true},
	}
	p := &picker{sc: &screen{cols: 80, rows: 24}}
	joined := view.StripANSI(strings.Join(p.netPanelBox(np), "\n"))
	if !strings.Contains(joined, "pending") || !strings.Contains(joined, "querying") {
		t.Fatalf("in-flight host should show a querying row:\n%s", joined)
	}
}

// The cursor moves within the roster and clamps at both ends.
func TestNetPanelCursorMoves(t *testing.T) {
	np := netTestState(
		onlineRow("a", 1, "in-sync"),
		onlineRow("b", 2, "in-sync"),
		onlineRow("c", 3, "in-sync"),
	)
	if np.cursor != 0 {
		t.Fatalf("cursor should start at 0, got %d", np.cursor)
	}
	np.move(1)
	if name, _ := np.current(); name != "b" {
		t.Fatalf("after one down, cursor should be on b, got %q", name)
	}
	np.move(10) // clamp at the last row
	if name, _ := np.current(); name != "c" {
		t.Fatalf("cursor should clamp to the last host, got %q", name)
	}
	np.move(-10) // clamp at the first row
	if name, _ := np.current(); name != "a" {
		t.Fatalf("cursor should clamp to the first host, got %q", name)
	}
}

// A mutating action (sync) must NOT call the sync path when the confirm is
// declined, and must call it exactly once when the confirm is accepted. This is
// the guardrail that stops the panel from overwriting a remote config silently.
func TestNetPanelSyncRequiresConfirm(t *testing.T) {
	calls := 0
	p := &picker{
		netSync: func(name string) string { calls++; return "applied" },
	}
	np := netTestState(onlineRow("h1", 5, "drift"))
	act := make(chan netActionResult, 1)

	// Declined: the sync path is never touched.
	p.confirmFn = func(string) bool { return false }
	p.netSyncSelected(np, act)
	if calls != 0 {
		t.Fatalf("sync must not run when the confirm is declined, ran %d times", calls)
	}
	select {
	case <-act:
		t.Fatalf("a declined sync must not report a result")
	default:
	}

	// Accepted: the sync path runs exactly once (drain act to sync with the goroutine).
	p.confirmFn = func(string) bool { return true }
	p.netSyncSelected(np, act)
	res := <-act
	if calls != 1 {
		t.Fatalf("accepted sync should run once, ran %d times", calls)
	}
	if !strings.Contains(res.status, "applied") || len(res.refresh) != 1 || res.refresh[0] != "h1" {
		t.Fatalf("sync result should report applied and re-query h1, got %+v", res)
	}
}

// Rollback is confirm-gated too: declined leaves the rollback path untouched.
func TestNetPanelRollbackRequiresConfirm(t *testing.T) {
	calls := 0
	p := &picker{
		netRollback: func(name string) string { calls++; return "rolled back" },
		confirmFn:   func(string) bool { return false },
	}
	np := netTestState(onlineRow("h1", 5, "drift"))
	act := make(chan netActionResult, 1)
	p.netRollbackSelected(np, act)
	if calls != 0 {
		t.Fatalf("rollback must not run when declined, ran %d times", calls)
	}

	p.confirmFn = func(string) bool { return true }
	p.netRollbackSelected(np, act)
	<-act
	if calls != 1 {
		t.Fatalf("accepted rollback should run once, ran %d times", calls)
	}
}
