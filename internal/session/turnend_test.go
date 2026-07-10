package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, lines string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTurnEndPi pins the pi turn-end predicate: an assistant message with
// stopReason "stop" ends the turn; "toolUse" means a tool call is in flight;
// "error"/"length" end it in an error state; a trailing user message means the
// agent has not answered yet.
func TestTurnEndPi(t *testing.T) {
	cases := []struct {
		name      string
		lines     string
		wantEnded bool
		wantErr   string
	}{
		{"final answer", `{"type":"session","id":"s1"}
{"type":"message","message":{"role":"user","content":"go"}}
{"type":"message","message":{"role":"assistant","stopReason":"stop","content":"done"}}
`, true, ""},
		{"tool call in flight", `{"type":"message","message":{"role":"user","content":"go"}}
{"type":"message","message":{"role":"assistant","stopReason":"toolUse"}}
`, false, ""},
		{"tool result awaiting next assistant", `{"type":"message","message":{"role":"assistant","stopReason":"toolUse"}}
{"type":"message","message":{"role":"toolResult"}}
`, false, ""},
		{"model error", `{"type":"message","message":{"role":"user"}}
{"type":"message","message":{"role":"assistant","stopReason":"error"}}
`, true, "model error"},
		{"length limit", `{"type":"message","message":{"role":"assistant","stopReason":"length"}}
`, true, "output length limit"},
		{"user prompt pending", `{"type":"message","message":{"role":"user","content":"go"}}
`, false, ""},
		{"no messages yet", `{"type":"session","id":"s1"}
`, false, ""},
	}
	for _, c := range cases {
		p := writeTranscript(t, c.lines)
		ended, reason := TurnEnd("pi", p)
		if ended != c.wantEnded || reason != c.wantErr {
			t.Errorf("%s: TurnEnd = (%v, %q), want (%v, %q)", c.name, ended, reason, c.wantEnded, c.wantErr)
		}
	}
}

// TestTurnEndCodex pins the codex turn-end predicate: a task_complete after
// the last task_started ends the turn; a bare task_started (or a new
// task_started after an old task_complete) means a turn is in flight;
// turn_aborted ends it in an error state.
func TestTurnEndCodex(t *testing.T) {
	cases := []struct {
		name      string
		lines     string
		wantEnded bool
		wantErr   string
	}{
		{"task complete", `{"type":"session_meta","payload":{"id":"c1"}}
{"type":"event_msg","payload":{"type":"task_started"}}
{"type":"event_msg","payload":{"type":"agent_message"}}
{"type":"event_msg","payload":{"type":"task_complete"}}
`, true, ""},
		{"task in flight", `{"type":"event_msg","payload":{"type":"task_started"}}
{"type":"event_msg","payload":{"type":"token_count"}}
`, false, ""},
		{"second turn in flight", `{"type":"event_msg","payload":{"type":"task_started"}}
{"type":"event_msg","payload":{"type":"task_complete"}}
{"type":"event_msg","payload":{"type":"task_started"}}
`, false, ""},
		{"aborted", `{"type":"event_msg","payload":{"type":"task_started"}}
{"type":"event_msg","payload":{"type":"turn_aborted"}}
`, true, "turn aborted"},
		{"no events yet", `{"type":"session_meta","payload":{"id":"c1"}}
`, false, ""},
		// Regression: on startup codex opens and closes a task around the injected
		// prompt before it has answered. That empty boot turn has a task_complete
		// but no agent_message and a null last_agent_message; concluding on it
		// marks a live, still-working worker done at launch (cursor at the prompt,
		// no assistant turn). It must read as still in flight.
		{"empty boot turn, no agent message", `{"type":"session_meta","payload":{"id":"c1"}}
{"type":"event_msg","payload":{"type":"task_started"}}
{"type":"event_msg","payload":{"type":"user_message","message":"reply CODEX_OK"}}
{"type":"event_msg","payload":{"type":"token_count"}}
{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":null}}
`, false, ""},
		// A real answer turn: the task_complete reports its agent message even when
		// the streamed agent_message events are absent, so it still ends the turn.
		{"task complete via last_agent_message", `{"type":"event_msg","payload":{"type":"task_started"}}
{"type":"event_msg","payload":{"type":"user_message","message":"reply CODEX_OK"}}
{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"CODEX_OK"}}
`, true, ""},
	}
	for _, c := range cases {
		p := writeTranscript(t, c.lines)
		ended, reason := TurnEnd("codex", p)
		if ended != c.wantEnded || reason != c.wantErr {
			t.Errorf("%s: TurnEnd = (%v, %q), want (%v, %q)", c.name, ended, reason, c.wantEnded, c.wantErr)
		}
	}
}

func TestTurnStartedAfter(t *testing.T) {
	marker := time.Date(2026, 7, 6, 7, 28, 59, 0, time.UTC)
	before := marker.Add(-time.Second).Format(time.RFC3339Nano)
	after := marker.Add(time.Second).Format(time.RFC3339Nano)

	codex := writeTranscript(t, `{"type":"event_msg","timestamp":"`+before+`","payload":{"type":"task_started"}}
{"type":"event_msg","timestamp":"`+before+`","payload":{"type":"turn_aborted"}}
{"type":"event_msg","timestamp":"`+after+`","payload":{"type":"task_started"}}
`)
	if !TurnStartedAfter("codex", codex, marker) {
		t.Fatal("codex task_started after marker was not detected")
	}
	if TurnStartedAfter("codex", codex, marker.Add(2*time.Second)) {
		t.Fatal("codex task_started before marker incorrectly reopened")
	}

	pi := writeTranscript(t, `{"type":"message","timestamp":"`+before+`","message":{"role":"assistant","stopReason":"error"}}
{"type":"message","timestamp":"`+after+`","message":{"role":"user","content":"continue"}}
`)
	if !TurnStartedAfter("pi", pi, marker) {
		t.Fatal("pi user message after marker was not detected")
	}
	if TurnStartedAfter("claude", pi, marker) {
		t.Fatal("hooked harness must not use hookless turn-start detection")
	}
}

// TestTurnEndOtherFormats: formats with a real lifecycle hook (claude) or no
// line transcript (opencode) never turn-end through this inference, and an
// unreadable path is a safe no.
func TestTurnEndOtherFormats(t *testing.T) {
	p := writeTranscript(t, `{"type":"assistant","message":{"role":"assistant"}}`+"\n")
	for _, f := range []string{"claude", "opencode", "unknown"} {
		if ended, _ := TurnEnd(f, p); ended {
			t.Errorf("format %q must not infer turn end", f)
		}
	}
	if ended, _ := TurnEnd("pi", filepath.Join(t.TempDir(), "missing.jsonl")); ended {
		t.Error("a missing transcript must not read as ended")
	}
}
