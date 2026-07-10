package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/state"
)

// writeClaudeTranscript drops a minimal claude transcript for id under a temp
// HOME so config's default glob (~/.claude/projects/*/*.jsonl) and its uuid IDRe
// both match it. The last assistant message is the session's final report.
func writeClaudeTranscript(t *testing.T, home, id, report string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"user","message":{"content":"go"},"timestamp":"2026-07-02T00:00:00Z"}` + "\n" +
		`{"type":"assistant","message":{"id":"a","model":"claude-opus-4-8","content":` +
		mustJSON(report) + `,"usage":{"output_tokens":6}},"timestamp":"2026-07-02T00:00:01Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(s string) string {
	// A JSON string literal; the report is plain text so quoting is enough.
	return `"` + s + `"`
}

// isolate points HOME, state, and config at a temp dir so config.Load returns
// the defaults, session.Index globs only our fixtures, and meta/state write
// under the temp tree, never the developer's real ~/.claude.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads USERPROFILE on Windows
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	return home
}

// a valid uuid-shaped id so the default claude IDRe extracts it from the path.
const fixtureID = "00000000-0000-0000-0000-000000000abc"

// TestCaptureResultOnConclude: an interactive worker's Stop-hook conclusion
// snapshots its final report (the last assistant message) and a 0 exit into the
// durable meta record, so `ax result` can print them without scraping the pane.
func TestCaptureResultOnConclude(t *testing.T) {
	home := isolate(t)
	writeClaudeTranscript(t, home, fixtureID, "shipped it: all tests green")
	if err := meta.Save(fixtureID, meta.Meta{Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}

	App{}.concludeOrIdle(fixtureID)

	if !state.Done(fixtureID) {
		t.Fatal("worker did not conclude into the done state")
	}
	m := meta.Load(fixtureID)
	if m.Result != "shipped it: all tests green" {
		t.Fatalf("captured Result = %q, want the last assistant message", m.Result)
	}
	if m.Exit == nil || *m.Exit != 0 {
		t.Fatalf("captured Exit = %v, want 0 for a clean interactive conclusion", m.Exit)
	}
}

// TestCaptureResultCloseOnDone: --close-on-done tears the session down, but the
// captured result, exit, and success outcome must all survive the teardown, so a
// control-layer caller reads the final output of a worker that closed itself.
func TestCaptureResultCloseOnDone(t *testing.T) {
	home := isolate(t)
	stubCloseSession(t)
	writeClaudeTranscript(t, home, fixtureID, "done and closing")
	if err := meta.Save(fixtureID, meta.Meta{Mode: "interactive", Task: "ship it", CloseOnDone: true}); err != nil {
		t.Fatal(err)
	}

	App{}.concludeOrIdle(fixtureID)

	m := meta.Load(fixtureID)
	if m.Result != "done and closing" {
		t.Fatalf("close-on-done clobbered Result = %q", m.Result)
	}
	if m.Exit == nil || *m.Exit != 0 {
		t.Fatalf("close-on-done Exit = %v, want 0", m.Exit)
	}
	if m.Outcome != "success" {
		t.Fatalf("close-on-done Outcome = %q, want success", m.Outcome)
	}
}

// TestCaptureResultHeadless: a headless run's process exit captures its final
// report and its real exit code, alongside the existing outcome, and never
// clobbers the failure outcome/reason on a non-zero exit.
func TestCaptureResultHeadless(t *testing.T) {
	home := isolate(t)
	writeClaudeTranscript(t, home, fixtureID, "the headless answer")
	if err := meta.Save(fixtureID, meta.Meta{Mode: "headless", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}

	ConcludeExit(fixtureID, 0, "all good\n", false)

	m := meta.Load(fixtureID)
	if m.Result != "the headless answer" {
		t.Fatalf("headless captured Result = %q", m.Result)
	}
	if m.Exit == nil || *m.Exit != 0 {
		t.Fatalf("headless Exit = %v, want 0", m.Exit)
	}
	if m.Outcome != "success" {
		t.Fatalf("headless Outcome = %q, want success", m.Outcome)
	}

	// A non-zero exit records the real exit code and preserves failure state.
	bad := "11111111-0000-0000-0000-000000000abc"
	writeClaudeTranscript(t, home, bad, "partial work before it broke")
	if err := meta.Save(bad, meta.Meta{Mode: "headless", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}
	ConcludeExit(bad, 3, "doing work\n\nError: boom\n", false)
	mb := meta.Load(bad)
	if mb.Exit == nil || *mb.Exit != 3 {
		t.Fatalf("failed headless Exit = %v, want 3", mb.Exit)
	}
	if mb.Outcome != "failure" || mb.FailReason != "Error: boom" {
		t.Fatalf("failed headless outcome/reason = %q/%q, want failure/Error: boom", mb.Outcome, mb.FailReason)
	}
}

// TestResultOutcome: outcome resolves from the recorded Meta.Outcome, else from
// the durable terminal marker, else pending.
func TestResultOutcome(t *testing.T) {
	isolate(t)
	if got := resultOutcome("x", meta.Meta{Outcome: "success"}); got != "success" {
		t.Fatalf("recorded outcome = %q, want success", got)
	}
	state.WriteHook("done-id", "done")
	if got := resultOutcome("done-id", meta.Meta{}); got != "success" {
		t.Fatalf("derived from done marker = %q, want success", got)
	}
	state.WriteHook("failed-id", "failed")
	if got := resultOutcome("failed-id", meta.Meta{}); got != "failure" {
		t.Fatalf("derived from failed marker = %q, want failure", got)
	}
	if got := resultOutcome("unknown", meta.Meta{}); got != "pending" {
		t.Fatalf("no signal = %q, want pending", got)
	}
}

func writeCodexReopenedPendingResultTranscript(t *testing.T, home, id string, markerAt time.Time, oldReport string) string {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "07", "06")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldTurn := markerAt.Add(-2 * time.Second).UTC().Format(time.RFC3339Nano)
	newTurn := markerAt.Add(2 * time.Second).UTC().Format(time.RFC3339Nano)
	content := fmt.Sprintf(`{"type":"session_meta","timestamp":%q,"payload":{"id":%q}}
{"type":"event_msg","timestamp":%q,"payload":{"type":"task_started"}}
{"type":"response_item","timestamp":%q,"payload":{"type":"message","role":"assistant","content":%s}}
{"type":"event_msg","timestamp":%q,"payload":{"type":"turn_aborted"}}
{"type":"event_msg","timestamp":%q,"payload":{"type":"task_started"}}
`, oldTurn, id, oldTurn, oldTurn, mustJSON(oldReport), oldTurn, newTurn)
	path := filepath.Join(dir, "rollout-2026-07-06T00-00-00-"+id+".jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendCodexAssistantResult(t *testing.T, path, report string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, `{"type":"response_item","timestamp":%q,"payload":{"type":"message","role":"assistant","content":%s}}
`, now, mustJSON(report)); err != nil {
		t.Fatal(err)
	}
}

func TestResultDoesNotHealOldReportDuringReopenedPendingTurn(t *testing.T) {
	home := isolate(t)
	id := "33333333-0000-0000-0000-000000000abc"
	markerAt := time.Now().Add(-time.Minute)
	path := writeCodexReopenedPendingResultTranscript(t, home, id, markerAt, "old assistant report")
	exit := 1
	if err := meta.Save(id, meta.Meta{
		Mode:       "headless",
		Harness:    "codex",
		Task:       "ship it",
		Outcome:    "failure",
		FailReason: "turn aborted",
		Result:     "old cached result",
		Exit:       &exit,
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteHook(id, "failed"); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(axdir.StatePath("hookstate"), id)
	if err := os.Chtimes(hook, markerAt, markerAt); err != nil {
		t.Fatal(err)
	}
	writeFreshLegacyLiveRecord(t, id)

	out := captureStdout(t, func() { App{}.Result([]string{id, "--json"}) })
	var got struct {
		Outcome string `json:"outcome"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result --json was not parseable: %v\n%s", err, out)
	}
	if got.Outcome != "pending" || got.Result != "" {
		t.Fatalf("result --json = outcome %q result %q, want pending with empty result", got.Outcome, got.Result)
	}
	if state.Terminal(id) {
		t.Fatal("terminal marker still present after result refreshed the reopened turn")
	}
	m := meta.Load(id)
	if m.Result != "" {
		t.Fatalf("meta Result was healed from the old turn: %q", m.Result)
	}

	appendCodexAssistantResult(t, path, "new assistant report")
	ConcludeExit(id, 0, "new run finished\n", false)
	m = meta.Load(id)
	if m.Outcome != "success" || m.Result != "new assistant report" {
		t.Fatalf("after conclude outcome/result = %q/%q, want success/new assistant report", m.Outcome, m.Result)
	}
}

// TestWaitFor covers the blocking semantics `ax wait` exposes: all-of vs any-of,
// success vs failure exit codes, and the timeout path.
func TestWaitFor(t *testing.T) {
	isolate(t)
	fast := time.Millisecond

	// --all: exit 0 only when every id concluded successfully.
	state.WriteHook("a", "done")
	state.WriteHook("b", "done")
	if got := waitFor([]string{"a", "b"}, true, 0, fast); got != 0 {
		t.Fatalf("all-success waitFor = %d, want 0", got)
	}

	// --all with a failed id: non-zero.
	state.WriteHook("c", "failed")
	if got := waitFor([]string{"a", "c"}, true, 0, fast); got != 1 {
		t.Fatalf("all-with-failure waitFor = %d, want 1", got)
	}

	// --any returns 0 as soon as one succeeds, even if another id never ran.
	if got := waitFor([]string{"a", "never"}, false, 0, fast); got != 0 {
		t.Fatalf("any-success waitFor = %d, want 0", got)
	}

	// --any with only a failed terminal id among the terminal ones: non-zero.
	if got := waitFor([]string{"c", "never"}, false, 0, fast); got != 1 {
		t.Fatalf("any-failure waitFor = %d, want 1", got)
	}

	// Timeout: an id that never reaches a terminal state returns 124.
	start := time.Now()
	if got := waitFor([]string{"never"}, true, 20*time.Millisecond, fast); got != 124 {
		t.Fatalf("timeout waitFor = %d, want 124", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took %s, expected to fire near 20ms", elapsed)
	}
}

func TestWaitForSetsAndClearsOwnerWaitMarker(t *testing.T) {
	isolate(t)
	t.Setenv("AX_SESSION_ID", "coord")

	done := make(chan int, 1)
	go func() {
		done <- waitFor([]string{"child"}, true, time.Second, time.Millisecond)
	}()
	deadline := time.Now().Add(time.Second)
	for !state.WaitingOnChildren("coord") {
		if time.Now().After(deadline) {
			t.Fatal("wait marker was not set while child was non-terminal")
		}
		time.Sleep(time.Millisecond)
	}
	state.WriteHook("child", "done")
	if got := <-done; got != 0 {
		t.Fatalf("waitFor = %d, want 0", got)
	}
	if state.WaitingOnChildren("coord") {
		t.Fatal("wait marker was not cleared after wait returned")
	}
}

// TestWaitAndResultThroughAlias: a caller holding only the id a launch printed
// must be able to wait on and read a session whose harness minted its own id
// after launch (codex): the adopt step leaves an alias, and wait/result/outcome
// resolve through it. The alias may appear mid-wait (adoption races the
// waiter), so waitFor must re-resolve on every poll.
func TestWaitAndResultThroughAlias(t *testing.T) {
	isolate(t)
	fast := time.Millisecond

	// Adoption already happened: the printed id resolves and the wait sees the
	// real session's terminal marker.
	if err := meta.SaveAlias("printed-1", "real-1"); err != nil {
		t.Fatal(err)
	}
	state.WriteHook("real-1", "done")
	if got := waitFor([]string{"printed-1"}, true, 0, fast); got != 0 {
		t.Fatalf("waitFor through alias = %d, want 0", got)
	}
	if got := resultOutcome(resolveID("printed-1"), meta.Load(resolveID("printed-1"))); got != "success" {
		t.Fatalf("result outcome through alias = %q, want success", got)
	}

	// Adoption happens WHILE the wait is running: the alias and the real
	// session's conclusion land mid-poll, and the waiter still unblocks.
	go func() {
		time.Sleep(15 * time.Millisecond)
		meta.SaveAlias("printed-2", "real-2")
		state.WriteHook("real-2", "done")
	}()
	if got := waitFor([]string{"printed-2"}, true, time.Second, fast); got != 0 {
		t.Fatalf("waitFor with a mid-wait adoption = %d, want 0", got)
	}
}

// TestWaitForMultiWaitsForAll: with --all it must not return until the last id is
// terminal; a still-running id keeps it blocked. Driven by flipping the marker
// from a goroutine so the poll loop is exercised for real.
func TestWaitForMultiWaitsForAll(t *testing.T) {
	isolate(t)
	state.WriteHook("first", "done")
	go func() {
		time.Sleep(15 * time.Millisecond)
		state.WriteHook("second", "done")
	}()
	if got := waitFor([]string{"first", "second"}, true, time.Second, time.Millisecond); got != 0 {
		t.Fatalf("multi-id all waitFor = %d, want 0 once both concluded", got)
	}
}

func stubRemoteWait(t *testing.T, fn remoteWaitFunc) {
	t.Helper()
	orig := runRemoteWait
	runRemoteWait = fn
	t.Cleanup(func() { runRemoteWait = orig })
}

func stubLocalWait(t *testing.T, fn func(context.Context, []string, bool, time.Duration) int) {
	t.Helper()
	orig := runLocalWait
	runLocalWait = fn
	t.Cleanup(func() { runLocalWait = orig })
}

func mustParseWait(t *testing.T, args []string) waitSpec {
	t.Helper()
	spec, err := parseWaitArgs(args)
	if err != nil {
		t.Fatalf("parseWaitArgs(%v): %v", args, err)
	}
	return spec
}

func TestWaitPlanHostFlagAppliesToPrecedingBareIDs(t *testing.T) {
	spec := mustParseWait(t, []string{"abc", "--host", "win01"})
	if len(spec.groups) != 1 {
		t.Fatalf("groups = %+v, want one remote group", spec.groups)
	}
	if spec.groups[0].host != "win01" {
		t.Fatalf("host = %q, want win01", spec.groups[0].host)
	}
	if len(spec.groups[0].ids) != 1 || spec.groups[0].ids[0] != "abc" {
		t.Fatalf("ids = %#v, want [abc]", spec.groups[0].ids)
	}
}

func TestWaitPlanHostFlagConflictsWithQualifiedIDBeforeFlag(t *testing.T) {
	if _, err := parseWaitArgs([]string{"mac01/abc", "--host", "win01"}); err == nil {
		t.Fatal("parseWaitArgs accepted host-qualified id that conflicts with trailing --host")
	}
}

func TestWaitPlanAllowsMixedLocalRemote(t *testing.T) {
	spec := mustParseWait(t, []string{"local-live", "win01/remote-done", "--any", "--timeout", "10m"})
	if len(spec.groups) != 2 {
		t.Fatalf("mixed local+remote wait should produce two groups, got %+v", spec.groups)
	}
	if spec.all {
		t.Fatal("--any was not preserved in the mixed wait plan")
	}
	if spec.timeout != 10*time.Minute {
		t.Fatalf("timeout = %s, want 10m", spec.timeout)
	}
}

func TestWaitForMixedAnySuccessDominatesConcurrentFailure(t *testing.T) {
	isolate(t)
	state.WriteHook("local-done", "done")
	spec := mustParseWait(t, []string{"win01/failed", "local-done", "--any"})
	localStarted := make(chan struct{})
	stubRemoteWait(t, func(ctx context.Context, host string, ids []string, all bool, timeout time.Duration) int {
		if host != "win01" || len(ids) != 1 || ids[0] != "failed" || all {
			return 99
		}
		return 1
	})
	stubLocalWait(t, func(ctx context.Context, ids []string, all bool, poll time.Duration) int {
		if len(ids) != 1 || ids[0] != "local-done" || all {
			return 99
		}
		close(localStarted)
		<-ctx.Done()
		return 0
	})

	if got := waitForMixed(spec, time.Millisecond); got != 0 {
		t.Fatalf("mixed --any concurrent success/failure = %d, want success 0", got)
	}
	select {
	case <-localStarted:
	default:
		t.Fatal("local wait was not invoked")
	}
}

func TestWaitForMixedAnyReturnsRemoteTerminalWhileLocalLive(t *testing.T) {
	isolate(t)
	spec := mustParseWait(t, []string{"local-live", "win01/remote-done", "--any", "--timeout", "1s"})
	remoteCalled := false
	stubRemoteWait(t, func(ctx context.Context, host string, ids []string, all bool, timeout time.Duration) int {
		remoteCalled = true
		if host != "win01" || len(ids) != 1 || ids[0] != "remote-done" || all || timeout != time.Second {
			return 99
		}
		return 0
	})

	if got := waitForMixed(spec, time.Millisecond); got != 0 {
		t.Fatalf("mixed --any waitFor = %d, want remote success 0", got)
	}
	if !remoteCalled {
		t.Fatal("remote wait was not invoked")
	}
	if state.Terminal("local-live") {
		t.Fatal("test setup expected local-live to remain non-terminal")
	}
}

func TestWaitForMixedAnyWaitsForLosingRemoteCleanup(t *testing.T) {
	isolate(t)
	state.WriteHook("local-done", "done")
	spec := mustParseWait(t, []string{"local-done", "win01/remote-live", "--any"})
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	release := func() {
		select {
		case <-releaseCleanup:
		default:
			close(releaseCleanup)
		}
	}
	defer release()
	stubRemoteWait(t, func(ctx context.Context, host string, ids []string, all bool, timeout time.Duration) int {
		<-ctx.Done()
		close(cleanupStarted)
		<-releaseCleanup
		return 124
	})

	done := make(chan int, 1)
	go func() {
		done <- waitForMixed(spec, time.Millisecond)
	}()

	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("remote wait did not observe cancellation")
	}
	select {
	case got := <-done:
		t.Fatalf("waitForMixed returned %d before losing remote cleanup finished", got)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	select {
	case got := <-done:
		if got != 0 {
			t.Fatalf("mixed --any waitFor = %d, want 0", got)
		}
	case <-time.After(time.Second):
		t.Fatal("waitForMixed did not return after remote cleanup finished")
	}
}

func TestWaitForMixedUnknownRemoteHostReturnsNormallyAndCleansUp(t *testing.T) {
	isolate(t)
	spec := mustParseWait(t, []string{"typo/remote-live", "local-live", "--any"})
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	release := func() {
		select {
		case <-releaseCleanup:
		default:
			close(releaseCleanup)
		}
	}
	defer release()
	stubLocalWait(t, func(ctx context.Context, ids []string, all bool, poll time.Duration) int {
		<-ctx.Done()
		close(cleanupStarted)
		<-releaseCleanup
		return 124
	})

	done := make(chan int, 1)
	go func() {
		done <- waitForMixed(spec, time.Millisecond)
	}()

	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("local wait did not observe cancellation after unknown remote host")
	}
	select {
	case got := <-done:
		t.Fatalf("waitForMixed returned %d before local cleanup finished", got)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	select {
	case got := <-done:
		if got != 1 {
			t.Fatalf("mixed wait with unknown remote host = %d, want normal error 1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("waitForMixed did not return after local cleanup finished")
	}
}

func TestWaitForMixedAllAggregatesLocalAndRemoteTerminalStates(t *testing.T) {
	t.Run("success waits for local and remote", func(t *testing.T) {
		isolate(t)
		spec := mustParseWait(t, []string{"local-late", "win01/remote-done", "--all", "--timeout", "1s"})
		stubRemoteWait(t, func(ctx context.Context, host string, ids []string, all bool, timeout time.Duration) int {
			if host != "win01" || !all {
				return 99
			}
			return 0
		})
		go func() {
			time.Sleep(15 * time.Millisecond)
			state.WriteHook("local-late", "done")
		}()

		if got := waitForMixed(spec, time.Millisecond); got != 0 {
			t.Fatalf("mixed --all waitFor = %d, want 0 after both groups complete", got)
		}
	})

	t.Run("remote failure fails aggregate", func(t *testing.T) {
		isolate(t)
		state.WriteHook("local-done", "done")
		spec := mustParseWait(t, []string{"local-done", "win01/remote-failed", "--all"})
		stubRemoteWait(t, func(ctx context.Context, host string, ids []string, all bool, timeout time.Duration) int {
			return 1
		})

		if got := waitForMixed(spec, time.Millisecond); got != 1 {
			t.Fatalf("mixed --all with remote failure = %d, want 1", got)
		}
	})
}

func TestWaitForMixedTimeoutReturns124(t *testing.T) {
	isolate(t)
	spec := mustParseWait(t, []string{"local-live", "win01/remote-live", "--all", "--timeout", "20ms"})
	stubRemoteWait(t, func(ctx context.Context, host string, ids []string, all bool, timeout time.Duration) int {
		<-ctx.Done()
		return 124
	})

	start := time.Now()
	if got := waitForMixed(spec, time.Millisecond); got != 124 {
		t.Fatalf("mixed timeout waitFor = %d, want 124", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("mixed timeout took %s, expected to fire near 20ms", elapsed)
	}
}

func TestWaitForMixedTimeoutWaitsForRemoteCleanup(t *testing.T) {
	isolate(t)
	spec := mustParseWait(t, []string{"local-live", "win01/remote-live", "--all", "--timeout", "20ms"})
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	release := func() {
		select {
		case <-releaseCleanup:
		default:
			close(releaseCleanup)
		}
	}
	defer release()
	stubRemoteWait(t, func(ctx context.Context, host string, ids []string, all bool, timeout time.Duration) int {
		<-ctx.Done()
		close(cleanupStarted)
		<-releaseCleanup
		return 124
	})

	done := make(chan int, 1)
	go func() {
		done <- waitForMixed(spec, time.Millisecond)
	}()

	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("remote wait did not observe timeout cancellation")
	}
	select {
	case got := <-done:
		t.Fatalf("waitForMixed returned %d before timeout cleanup finished", got)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	select {
	case got := <-done:
		if got != 124 {
			t.Fatalf("mixed timeout waitFor = %d, want 124", got)
		}
	case <-time.After(time.Second):
		t.Fatal("waitForMixed did not return after timeout cleanup finished")
	}
}

// TestCaptureResultFastConclude: when a fast close-on-done worker's Stop hook
// fires before the harness re-logs the complete final message, CaptureResult
// must capture the full body, not just the preamble from the streaming partial.
//
// The transcript starts with only a streaming partial record (preamble text, no
// output_tokens). A goroutine appends the complete re-log (full body, with
// output_tokens) after a short delay, simulating the harness's async write.
// CaptureResult's retry loop must wait for it and store the full body.
func TestCaptureResultFastConclude(t *testing.T) {
	home := isolate(t)

	id := "ffffffff-0000-0000-0000-000000000fff"
	dir := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")

	// Streaming partial: preamble text, no usage tokens.
	partial := `{"type":"user","message":{"content":"go"},"timestamp":"2026-07-02T00:00:00Z"}` + "\n" +
		`{"type":"assistant","message":{"id":"a","model":"claude-opus-4-8","content":"Preamble only."},"timestamp":"2026-07-02T00:00:01Z"}` + "\n"
	if err := os.WriteFile(path, []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := meta.Save(id, meta.Meta{Mode: "interactive", Task: "ship it"}); err != nil {
		t.Fatal(err)
	}

	// Complete re-log arrives after ~80ms (within the retry window of 5×50ms=250ms).
	fullBody := "Preamble only.\n\nFull body here with all the details."
	go func() {
		time.Sleep(80 * time.Millisecond)
		relog := `{"type":"assistant","message":{"id":"a","model":"claude-opus-4-8","content":` +
			`"Preamble only.\n\nFull body here with all the details."` +
			`,"usage":{"output_tokens":12}},"timestamp":"2026-07-02T00:00:02Z"}` + "\n"
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		f.WriteString(relog)
	}()

	CaptureResult(id, 0)

	m := meta.Load(id)
	if m.Result != fullBody {
		t.Fatalf("fast-conclude captured Result = %q, want full body %q", m.Result, fullBody)
	}
}
