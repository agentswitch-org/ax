package follow

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/session"
)

func appendClaudeTurn(t *testing.T, path, role, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer f.Close()
	line := fmt.Sprintf(`{"type":%q,"timestamp":"2026-07-02T00:00:00Z","message":{"role":%q,"content":%q,"usage":{"output_tokens":1}}}`+"\n", role, role, text)
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	old := time.Now().Add(-live.Active - time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("backdate transcript: %v", err)
	}
}

func waitEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for follow event")
		return Event{}
	}
}

func freshSnap(id string) func() map[string]live.Entry {
	return func() map[string]live.Entry {
		return map[string]live.Entry{id: {Age: 0}}
	}
}

// --- emit ---

func TestEmitSendsToChannel(t *testing.T) {
	ctx := context.Background()
	ch := make(chan Event, 1)
	e := Event{ID: "s1", Event: "turn"}
	if !emit(ctx, ch, e) {
		t.Fatal("emit should return true on success")
	}
	got := <-ch
	if got.ID != "s1" || got.Event != "turn" {
		t.Errorf("unexpected event: %+v", got)
	}
}

func TestEmitCancelledContextReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan Event) // unbuffered: would block without ctx guard
	if emit(ctx, ch, Event{Event: "turn"}) {
		t.Fatal("emit should return false when context is already cancelled")
	}
}

func TestEmitFullChannelCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan Event, 1)
	ch <- Event{Event: "existing"} // fill the buffer
	if emit(ctx, ch, Event{Event: "new"}) {
		t.Fatal("emit should return false when channel is full and context is cancelled")
	}
}

func TestStreamStartCursorSkipsHistoricalAndNamesWorker(t *testing.T) {
	path := t.TempDir() + "/worker.jsonl"
	appendClaudeTurn(t, path, "assistant", "historical replay noise")
	_, cursor := session.Turns("claude", path, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Event, 4)
	go Stream(ctx, Target{
		ID: "worker-1", Name: "builder", Group: "run-1", Format: "claude", File: path,
		StartCursor: cursor, UseStartCursor: true,
	}, Options{Snap: freshSnap("worker-1")}, out)

	appendClaudeTurn(t, path, "assistant", "fresh progress")
	ev := waitEvent(t, out)
	if ev.Event != "turn" || ev.ID != "worker-1" || ev.Name != "builder" || ev.Group != "run-1" {
		t.Fatalf("event identity = %+v, want named worker turn in run-1", ev)
	}
	if ev.Preview != "fresh progress" {
		t.Fatalf("preview = %q, want fresh progress without historical replay", ev.Preview)
	}
	if ev.Cursor != cursor+1 {
		t.Fatalf("cursor = %d, want %d", ev.Cursor, cursor+1)
	}
}

func TestStreamWaitingEventIncludesWorkerName(t *testing.T) {
	path := t.TempDir() + "/worker.jsonl"
	appendClaudeTurn(t, path, "assistant", "ready for input")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Event, 4)
	go Stream(ctx, Target{
		ID: "worker-2", Name: "reviewer", Group: "run-1", Format: "claude", File: path,
		WaitingRe: "selection",
	}, Options{
		Snap:     freshSnap("worker-2"),
		PaneTail: func(string) string { return "Enter your selection:\n> " },
	}, out)

	ev := waitEvent(t, out)
	if ev.Event != "waiting" || ev.ID != "worker-2" || ev.Name != "reviewer" {
		t.Fatalf("event identity = %+v, want named waiting worker", ev)
	}
	if ev.Reason != "input" || ev.Hint != ">" {
		t.Fatalf("waiting classification = reason %q hint %q, want input >", ev.Reason, ev.Hint)
	}
}

func TestStreamExitEventIncludesWorkerName(t *testing.T) {
	var calls int32
	snap := func() map[string]live.Entry {
		if atomic.AddInt32(&calls, 1) == 1 {
			return map[string]live.Entry{"worker-3": {Age: 0}}
		}
		return map[string]live.Entry{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Event, 4)
	go Stream(ctx, Target{ID: "worker-3", Name: "finisher", Group: "run-1"}, Options{Snap: snap}, out)

	ev := waitEvent(t, out)
	if ev.Event != "exit" || ev.ID != "worker-3" || ev.Name != "finisher" || ev.Group != "run-1" {
		t.Fatalf("event identity = %+v, want named exit worker", ev)
	}
}

// --- Options.wants ---

func TestWantsNilEventsExcludesOutput(t *testing.T) {
	o := Options{Events: nil}
	if o.wants("output") {
		t.Error("nil Events should exclude 'output' (opt-in only)")
	}
}

func TestWantsNilEventsIncludesDefaultEvents(t *testing.T) {
	o := Options{Events: nil}
	for _, ev := range []string{"turn", "waiting", "exit", "crash"} {
		if !o.wants(ev) {
			t.Errorf("nil Events should include %q by default", ev)
		}
	}
}

func TestWantsExplicitSetAllowed(t *testing.T) {
	o := Options{Events: map[string]bool{"turn": true, "output": true}}
	if !o.wants("turn") {
		t.Error("should want 'turn'")
	}
	if !o.wants("output") {
		t.Error("should want 'output' when explicitly set")
	}
}

func TestWantsExplicitSetExcludes(t *testing.T) {
	o := Options{Events: map[string]bool{"turn": true}}
	if o.wants("exit") {
		t.Error("'exit' should not be wanted when not in explicit set")
	}
	if o.wants("output") {
		t.Error("'output' should not be wanted when not in explicit set")
	}
}

// --- waiting ---

func TestWaitingEmptyWaitingRe(t *testing.T) {
	tgt := Target{WaitingRe: ""}
	opt := Options{PaneTail: func(string) string { return "something" }}
	reason, hint := waiting(tgt, opt)
	if reason != "" || hint != "" {
		t.Errorf("empty WaitingRe should return empty, got reason=%q hint=%q", reason, hint)
	}
}

func TestWaitingNilPaneTail(t *testing.T) {
	tgt := Target{WaitingRe: "."}
	opt := Options{PaneTail: nil}
	reason, hint := waiting(tgt, opt)
	if reason != "" || hint != "" {
		t.Errorf("nil PaneTail should return empty, got reason=%q hint=%q", reason, hint)
	}
}

func TestWaitingInvalidRegex(t *testing.T) {
	tgt := Target{WaitingRe: "[unclosed"}
	opt := Options{PaneTail: func(string) string { return "abc" }}
	reason, hint := waiting(tgt, opt)
	if reason != "" || hint != "" {
		t.Errorf("invalid regex should return empty, got reason=%q hint=%q", reason, hint)
	}
}

func TestWaitingNoMatch(t *testing.T) {
	tgt := Target{WaitingRe: "PROMPT"}
	opt := Options{PaneTail: func(string) string { return "idle output" }}
	reason, hint := waiting(tgt, opt)
	if reason != "" || hint != "" {
		t.Errorf("non-matching tail should return empty, got reason=%q hint=%q", reason, hint)
	}
}

func TestWaitingAuthURL(t *testing.T) {
	tail := "Please authenticate at http://auth.example.com/login to continue"
	tgt := Target{WaitingRe: "authenticate"}
	opt := Options{PaneTail: func(string) string { return tail }}
	reason, hint := waiting(tgt, opt)
	if reason != "auth" {
		t.Errorf("URL in tail should yield reason=auth, got %q", reason)
	}
	if !strings.HasPrefix(hint, "http") {
		t.Errorf("hint should be the URL, got %q", hint)
	}
}

func TestWaitingInputPrompt(t *testing.T) {
	tail := "Enter your selection:\n> "
	tgt := Target{WaitingRe: "selection"}
	opt := Options{PaneTail: func(string) string { return tail }}
	reason, hint := waiting(tgt, opt)
	if reason != "input" {
		t.Errorf("non-URL tail should yield reason=input, got %q", reason)
	}
	if hint == "" {
		t.Error("hint should be the last non-empty line of the tail")
	}
}

// --- preview ---

func TestPreviewEmptyTurns(t *testing.T) {
	got := preview(nil, false)
	if got != "" {
		t.Errorf("want empty string, got %q", got)
	}
}

func TestPreviewShortText(t *testing.T) {
	turns := []session.NormTurn{{Text: "hello world"}}
	got := preview(turns, false)
	if got != "hello world" {
		t.Errorf("want %q, got %q", "hello world", got)
	}
}

func TestPreviewNormalizesWhitespace(t *testing.T) {
	turns := []session.NormTurn{{Text: "  hello   world  "}}
	got := preview(turns, false)
	if got != "hello world" {
		t.Errorf("whitespace should be normalized, got %q", got)
	}
}

func TestPreviewLongTextTruncated(t *testing.T) {
	long := strings.Repeat("x", 300)
	turns := []session.NormTurn{{Text: long}}
	got := preview(turns, false)
	runes := []rune(got)
	if len(runes) > 200 {
		t.Errorf("long text should be truncated to <=200 runes, got %d", len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated text should end with ellipsis, suffix: %q", got[len(got)-5:])
	}
}

func TestPreviewWithContentNoTruncation(t *testing.T) {
	long := strings.Repeat("y", 300)
	turns := []session.NormTurn{{Text: long}}
	got := preview(turns, true)
	if len(got) < 300 {
		t.Errorf("withContent=true should not truncate, got len %d", len(got))
	}
}

func TestPreviewUsesLastTurn(t *testing.T) {
	turns := []session.NormTurn{
		{Text: "first"},
		{Text: "second"},
		{Text: "last"},
	}
	got := preview(turns, false)
	if got != "last" {
		t.Errorf("preview should use last turn's text, got %q", got)
	}
}

func TestPreviewExactly200RunesNotTruncated(t *testing.T) {
	text := strings.Repeat("a", 200)
	turns := []session.NormTurn{{Text: text}}
	got := preview(turns, false)
	if len(got) != 200 {
		t.Errorf("exactly 200 chars should not be truncated, got len %d", len(got))
	}
}

// --- firstURL ---

func TestFirstURLExtractsHTTPS(t *testing.T) {
	got := firstURL("Go to https://auth.example.com/login to sign in")
	if got != "https://auth.example.com/login" {
		t.Errorf("want URL, got %q", got)
	}
}

func TestFirstURLExtractsHTTP(t *testing.T) {
	got := firstURL("visit http://example.com for details")
	if got != "http://example.com" {
		t.Errorf("want http URL, got %q", got)
	}
}

func TestFirstURLFallsBackToLastLine(t *testing.T) {
	got := firstURL("line one\nlast line")
	if got != "last line" {
		t.Errorf("no URL: should fall back to last line, got %q", got)
	}
}

// --- lastLine ---

func TestLastLineSingleLine(t *testing.T) {
	got := lastLine("hello")
	if got != "hello" {
		t.Errorf("want 'hello', got %q", got)
	}
}

func TestLastLineMultiLine(t *testing.T) {
	got := lastLine("foo\nbar\nbaz")
	if got != "baz" {
		t.Errorf("want 'baz', got %q", got)
	}
}

func TestLastLineTrailingNewlines(t *testing.T) {
	got := lastLine("foo\nbar\n\n")
	if got != "bar" {
		t.Errorf("trailing newlines should be skipped, got %q", got)
	}
}

func TestLastLineAllEmpty(t *testing.T) {
	got := lastLine("\n\n\n")
	if got != "" {
		t.Errorf("all-empty should return empty string, got %q", got)
	}
}

func TestLastLineTrimsSpaces(t *testing.T) {
	got := lastLine("foo\n   bar   ")
	if got != "bar" {
		t.Errorf("should trim surrounding spaces, got %q", got)
	}
}

func TestLastLineEmpty(t *testing.T) {
	got := lastLine("")
	if got != "" {
		t.Errorf("empty input should return empty string, got %q", got)
	}
}
