package session

import (
	"encoding/json"
	"io"
	"os"
	"strings"
)

// LastReport returns the text of a transcript's last assistant message: the
// session's final report. It reuses Turns, so it normalizes across harnesses,
// and returns the last assistant turn that carries actual text (skipping a
// trailing tool-only turn). Empty when the transcript is unreadable or has no
// assistant text. This is the interactive equivalent of the final answer a
// headless `claude -p` prints to stdout.
func LastReport(format, path string) string {
	t, _ := LastReportFull(format, path)
	return t
}

// LastReportFull is like LastReport but also returns whether the last assistant
// turn's usage was flushed (output_tokens > 0). A turn with output_tokens == 0
// is a streaming partial written before the harness re-logged the completed
// message; complete=false signals the caller to retry.
func LastReportFull(format, path string) (text string, complete bool) {
	turns, _ := Turns(format, path, 0)
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role != "assistant" {
			continue
		}
		if t := strings.TrimSpace(turns[i].Text); t != "" {
			return t, turns[i].Tokens.Out > 0
		}
	}
	return "", false
}

// NormTurn is a normalized conversation turn: one user, assistant, or tool
// message, harness-agnostic. Seq is the transcript record index it ends at, so a
// cursor is a plain record count that any parser can produce cheaply.
type NormTurn struct {
	Seq    int        `json:"seq"`
	Role   string     `json:"role"`
	Model  string     `json:"model,omitempty"`
	Ts     string     `json:"ts,omitempty"`
	Text   string     `json:"text,omitempty"`
	Tokens NormTokens `json:"tokens"`
}

// NormTokens is a turn's token usage (assistant turns; zero otherwise).
type NormTokens struct {
	In         int `json:"in"`
	Out        int `json:"out"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

// Turns extracts normalized turns from a transcript, assigning each record a
// sequential seq. It returns the turns with seq > since and the new cursor (the
// total record count), so `ax read --since N` streams only what is new. Unknown
// or db-backed formats return no turns.
func Turns(format, path string, since int) ([]NormTurn, int) {
	f, err := os.Open(path)
	if err != nil {
		return nil, since
	}
	defer f.Close()
	dec := json.NewDecoder(f)

	var out []NormTurn
	cursor := 0
	emit := func(role, model, ts, text string, tok NormTokens) {
		cursor++
		if cursor > since && (text != "" || tok.Out > 0) {
			out = append(out, NormTurn{Seq: cursor, Role: role, Model: model, Ts: ts, Text: text, Tokens: tok})
		}
	}
	skip := func() { cursor++ }

	for {
		switch format {
		case "claude":
			var r claudeRec
			if !decodeRec(dec, &r) {
				return out, cursor
			}
			if r.Type != "user" && r.Type != "assistant" {
				skip()
				continue
			}
			var m claudeMsg
			json.Unmarshal(r.Message, &m)
			emit(r.Type, m.Model, r.Timestamp, extractText(m.Content), NormTokens{
				In: m.Usage.Input, Out: m.Usage.Output,
				CacheRead: m.Usage.CacheRead, CacheWrite: m.Usage.CacheWrite,
			})
		case "pi":
			var r piRec
			if !decodeRec(dec, &r) {
				return out, cursor
			}
			if r.Type != "message" {
				skip()
				continue
			}
			var m piMsg
			json.Unmarshal(r.Message, &m)
			emit(m.Role, m.Model, r.Timestamp, extractText(m.Content), NormTokens{
				In: m.Usage.Input, Out: m.Usage.Output,
				CacheRead: m.Usage.CacheRead, CacheWrite: m.Usage.CacheWrite,
			})
		case "codex":
			var r codexRec
			if !decodeRec(dec, &r) {
				return out, cursor
			}
			if r.Type != "response_item" {
				skip()
				continue
			}
			var p struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			json.Unmarshal(r.Payload, &p)
			emit(p.Role, "", r.Timestamp, extractText(p.Content), NormTokens{})
		default:
			return out, cursor
		}
	}
}

// decodeRec decodes one JSONL record, skipping a single bad token so a corrupt
// line does not end the read early. Returns false at EOF or an unrecoverable
// error.
func decodeRec(dec *json.Decoder, v any) bool {
	err := dec.Decode(v)
	if err == nil {
		return true
	}
	if err == io.EOF {
		return false
	}
	return skipBadToken(dec)
}
