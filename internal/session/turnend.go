package session

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// TurnEnd reports whether a transcript's most recent agent turn has ended, for
// harnesses that expose no lifecycle hook (pi, codex): their own transcript is
// the only authoritative completion signal, so the run wrapper watches it and
// concludes a task-carrying interactive worker the same way claude's Stop hook
// does. errReason is non-empty when the turn ended in an error state (a model
// error, an aborted turn), so the caller can conclude failed instead of done.
//
//   - pi appends one complete "message" record per message; an assistant
//     message with stopReason "stop" is the turn's final answer ("toolUse"
//     means a tool call is in flight, so the turn is still going).
//   - codex appends event_msg records; a "task_complete" after the last
//     "task_started" means the turn ended ("turn_aborted" ends it in error).
//
// Formats with a real lifecycle hook (claude) or no line-oriented transcript
// (opencode) return false: the hook, not this inference, is authoritative there.
func TurnEnd(format, path string) (ended bool, errReason string) {
	f, err := os.Open(path)
	if err != nil {
		return false, ""
	}
	defer f.Close()
	switch format {
	case "pi":
		return piTurnEnd(f)
	case "codex":
		return codexTurnEnd(f)
	}
	return false, ""
}

// TurnStartedAfter reports whether the transcript contains a new user/task turn
// start after after. It is deliberately narrower than "any activity": it is used
// to reopen a live same-session lifecycle only when the harness records that a
// later turn began after a durable done/failed marker was written.
func TurnStartedAfter(format, path string, after time.Time) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	switch format {
	case "pi":
		return piTurnStartedAfter(f, after)
	case "codex":
		return codexTurnStartedAfter(f, after)
	}
	return false
}

func piTurnStartedAfter(r io.Reader, after time.Time) bool {
	dec := json.NewDecoder(r)
	for {
		var rec struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			if !skipBadToken(dec) {
				break
			}
			continue
		}
		if rec.Type == "message" && rec.Message.Role == "user" && parseTime(rec.Timestamp).After(after) {
			return true
		}
	}
	return false
}

func codexTurnStartedAfter(r io.Reader, after time.Time) bool {
	dec := json.NewDecoder(r)
	for {
		var rec struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Type string `json:"type"`
			} `json:"payload"`
		}
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			if !skipBadToken(dec) {
				break
			}
			continue
		}
		if rec.Type == "event_msg" && rec.Payload.Type == "task_started" && parseTime(rec.Timestamp).After(after) {
			return true
		}
	}
	return false
}

// piTurnEnd scans a pi transcript for the state of its last message: an
// assistant message with stopReason "stop" ends the turn cleanly; "error" and
// "length" end it in an error state; anything else (a user message, an
// assistant "toolUse") means the turn is still in flight.
func piTurnEnd(r io.Reader) (bool, string) {
	dec := json.NewDecoder(r)
	lastRole, lastStop := "", ""
	sawMessage := false
	for {
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Role       string `json:"role"`
				StopReason string `json:"stopReason"`
			} `json:"message"`
		}
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			if !skipBadToken(dec) {
				break
			}
			continue
		}
		if rec.Type != "message" {
			continue
		}
		sawMessage = true
		lastRole, lastStop = rec.Message.Role, rec.Message.StopReason
	}
	if !sawMessage || lastRole != "assistant" {
		return false, ""
	}
	switch lastStop {
	case "stop":
		return true, ""
	case "error":
		return true, "model error"
	case "length":
		return true, "output length limit"
	}
	return false, "" // toolUse or unknown: the turn is still going
}

// codexTurnEnd scans a codex rollout for its task lifecycle events: the turn
// has ended when the last task_started is followed by a task_complete (clean)
// or a turn_aborted (error).
//
// A clean task_complete only counts when its turn actually produced an agent
// message. On startup codex opens a task (task_started) and closes it
// (task_complete) around the injected prompt before it has answered; that empty
// boot turn carries no agent_message event and a null last_agent_message.
// Concluding on it would mark a freshly launched worker done at launch, before
// any assistant turn, so `ax wait`/`ax result` return success against nothing.
// Requiring an agent message keeps a live, still-working codex worker live.
func codexTurnEnd(r io.Reader) (bool, string) {
	dec := json.NewDecoder(r)
	last := ""          // last lifecycle event seen: task_started | task_complete | turn_aborted
	agentSpoke := false // an agent produced a message since the last task_started
	for {
		var rec struct {
			Type    string `json:"type"`
			Payload struct {
				Type             string `json:"type"`
				LastAgentMessage string `json:"last_agent_message"`
			} `json:"payload"`
		}
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			if !skipBadToken(dec) {
				break
			}
			continue
		}
		if rec.Type != "event_msg" {
			continue
		}
		switch rec.Payload.Type {
		case "task_started":
			last, agentSpoke = rec.Payload.Type, false
		case "turn_aborted":
			last = rec.Payload.Type
		case "task_complete":
			last = rec.Payload.Type
			if strings.TrimSpace(rec.Payload.LastAgentMessage) != "" {
				agentSpoke = true
			}
		case "agent_message":
			agentSpoke = true
		}
	}
	switch last {
	case "task_complete":
		if agentSpoke {
			return true, ""
		}
		return false, "" // an empty boot turn: not a genuine end, the worker is still live
	case "turn_aborted":
		return true, "turn aborted"
	}
	return false, ""
}
