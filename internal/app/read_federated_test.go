package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/follow"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/session"
)

func stubRemoteReadOnce(t *testing.T, fn remoteReadOnceFunc) {
	t.Helper()
	orig := runRemoteReadOnce
	runRemoteReadOnce = fn
	t.Cleanup(func() { runRemoteReadOnce = orig })
}

func stubRemoteReadStream(t *testing.T, fn remoteReadStreamFunc) {
	t.Helper()
	orig := streamRemoteRead
	streamRemoteRead = fn
	t.Cleanup(func() { streamRemoteRead = orig })
}

func writeReadHostConfig(t *testing.T, home, host string) {
	t.Helper()
	cfgPath := filepath.Join(home, "config.toml")
	body := "[[host]]\nname = \"" + host + "\"\ntransport = \"ssh " + host + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", cfgPath)
}

func writeReadHostsConfig(t *testing.T, home string, hosts ...string) {
	t.Helper()
	cfgPath := filepath.Join(home, "config.toml")
	var body strings.Builder
	for _, h := range hosts {
		body.WriteString("[[host]]\nname = \"" + h + "\"\ntransport = \"ssh " + h + "\"\n")
	}
	if err := os.WriteFile(cfgPath, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", cfgPath)
}

func appendFreshClaudeTurn(t *testing.T, home, id, text string) {
	t.Helper()
	path := filepath.Join(home, ".claude", "projects", "proj", id+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	line := `{"type":"assistant","message":{"id":"a","model":"claude-opus-4-8","content":` +
		mustJSON(text) + `,"usage":{"output_tokens":1}},"timestamp":"2026-07-02T00:00:02Z"}` + "\n"
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

func decodeRowsForTest(t *testing.T, out string) []readRow {
	t.Helper()
	rows, err := decodeReadRows([]byte(out))
	if err != nil {
		t.Fatalf("decode rows: %v\n%s", err, out)
	}
	return rows
}

func decodeEventsForTest(t *testing.T, out string) []follow.Event {
	t.Helper()
	dec := json.NewDecoder(bytes.NewBufferString(out))
	var events []follow.Event
	for {
		var ev follow.Event
		if err := dec.Decode(&ev); err != nil {
			if strings.Contains(err.Error(), "EOF") {
				return events
			}
			t.Fatalf("decode event: %v\n%s", err, out)
		}
		events = append(events, ev)
	}
}

func TestReadFederatedOnceActiveMergesLocalAndRemote(t *testing.T) {
	home := isolate(t)
	writeReadHostConfig(t, home, "win01")
	localID := "00000000-0000-0000-0000-000000000111"
	writeClaudeTranscript(t, home, localID, "local fresh")
	if err := meta.Save(localID, meta.Meta{Name: "local-worker", Group: "run-fed"}); err != nil {
		t.Fatal(err)
	}
	writeLegacyLive(t, localID, "claude")

	var gotArgs []string
	stubRemoteReadOnce(t, func(_ context.Context, h config.Host, args []string) ([]readRow, error) {
		if h.Name != "win01" {
			t.Fatalf("host = %q, want win01", h.Name)
		}
		gotArgs = append([]string(nil), args...)
		return []readRow{{
			ID:    "remote-1",
			Name:  "remote-worker",
			Group: "run-fed",
			Turns: []session.NormTurn{{
				Seq: 7, Role: "assistant", Ts: "2026-07-02T00:00:01Z", Text: "remote fresh",
				Tokens: session.NormTokens{Out: 1},
			}},
			Cursor: 7,
		}}, nil
	})

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Read([]string{"--run", "run-fed", "--active", "--hosts", "--limit", "1"})
	})
	rows := decodeRowsForTest(t, out)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want local + remote", rows)
	}
	byID := map[string]readRow{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	local := byID[localID]
	if local.Host != "local" || local.Name != "local-worker" || local.Group != "run-fed" || len(local.Turns) != 1 {
		t.Fatalf("local row identity/turns = %+v", local)
	}
	remote := byID["win01/remote-1"]
	if remote.Host != "win01" || remote.Name != "remote-worker" || remote.Group != "run-fed" || len(remote.Turns) != 1 {
		t.Fatalf("remote row identity/turns = %+v", remote)
	}
	wantArgs := []string{"--run", "run-fed", "--active", "--limit", "1", "--identity"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("remote args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestReadFederatedFollowFromNowFansOutAndSkipsStaleReplay(t *testing.T) {
	home := isolate(t)
	writeReadHostConfig(t, home, "win01")
	localID := "00000000-0000-0000-0000-000000000222"
	writeClaudeTranscript(t, home, localID, "stale local")
	if err := meta.Save(localID, meta.Meta{Name: "local-worker", Group: "run-fed"}); err != nil {
		t.Fatal(err)
	}
	writeLegacyLive(t, localID, "claude")

	stubRemoteReadStream(t, func(ctx context.Context, h config.Host, args []string, out chan<- federatedReadEvent) {
		if h.Name != "win01" {
			t.Fatalf("host = %q, want win01", h.Name)
		}
		wantArgs := []string{"--run", "run-fed", "--follow", "--active", "--from-now", "--events", "output", "--limit", "2", "--timeout", "3s"}
		if !reflect.DeepEqual(args, wantArgs) {
			t.Fatalf("remote follow args = %#v, want %#v", args, wantArgs)
		}
		select {
		case out <- federatedReadEvent{host: h.Name, event: follow.Event{
			ID: "remote-1", Name: "remote-worker", Group: "run-fed", Event: "output", Cursor: 4, Preview: "fresh remote",
		}}:
		case <-ctx.Done():
			return
		}
		time.Sleep(100 * time.Millisecond)
		appendFreshClaudeTurn(t, home, localID, "fresh local")
	})

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Read([]string{
			"--run", "run-fed", "--hosts", "--follow", "--active", "--from-now",
			"--events", "output", "--limit", "2", "--timeout", "3s",
		})
	})
	if strings.Contains(out, "stale local") || strings.Contains(out, "stale remote") {
		t.Fatalf("from-now replayed stale data:\n%s", out)
	}
	events := decodeEventsForTest(t, out)
	if len(events) != 2 {
		t.Fatalf("events = %+v, want remote + local", events)
	}
	seen := map[string]follow.Event{}
	for _, ev := range events {
		seen[ev.ID] = ev
	}
	if ev := seen["win01/remote-1"]; ev.Host != "win01" || ev.Name != "remote-worker" || ev.Group != "run-fed" || ev.Preview != "fresh remote" {
		t.Fatalf("remote event = %+v", ev)
	}
	if ev := seen[localID]; ev.Host != "local" || ev.Name != "local-worker" || ev.Group != "run-fed" || ev.Preview != "fresh local" {
		t.Fatalf("local event = %+v", ev)
	}
}

func TestReadFederatedFollowTimeoutNoEventOutputsNothing(t *testing.T) {
	home := isolate(t)
	writeReadHostConfig(t, home, "win01")
	stubRemoteReadStream(t, func(ctx context.Context, _ config.Host, _ []string, _ chan<- federatedReadEvent) {
		<-ctx.Done()
	})

	started := time.Now()
	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Read([]string{
			"--run", "run-fed", "--hosts", "--follow", "--active", "--from-now",
			"--limit", "1", "--timeout", "20ms",
		})
	})
	if out != "" {
		t.Fatalf("timeout with no event should emit no output, got:\n%s", out)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout took %s, want bounded by the requested timeout", elapsed)
	}
}

func TestReadRunDefaultLocalOnlyDoesNotFanOut(t *testing.T) {
	home := isolate(t)
	writeReadHostConfig(t, home, "win01")
	localID := "00000000-0000-0000-0000-000000000333"
	writeClaudeTranscript(t, home, localID, "local only")
	if err := meta.Save(localID, meta.Meta{Name: "local-worker", Group: "run-fed"}); err != nil {
		t.Fatal(err)
	}

	calls := 0
	stubRemoteReadOnce(t, func(context.Context, config.Host, []string) ([]readRow, error) {
		calls++
		return nil, nil
	})
	stubRemoteReadStream(t, func(context.Context, config.Host, []string, chan<- federatedReadEvent) {
		calls++
	})

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Read([]string{"--run", "run-fed", "--limit", "1"})
	})
	if calls != 0 {
		t.Fatalf("default read fanned out %d time(s); it must stay local-only", calls)
	}
	if strings.Contains(out, `"host"`) || strings.Contains(out, `"name"`) || strings.Contains(out, `"group"`) {
		t.Fatalf("local-only read changed its JSON identity shape:\n%s", out)
	}
}

// A one-shot federated read must isolate a failing host: the offline host is
// reported on stderr and contributes no rows, while the aggregate still returns
// the local session plus every healthy host.
func TestReadFederatedOnceHostErrorIsolatedKeepsLocalAndOtherHosts(t *testing.T) {
	home := isolate(t)
	writeReadHostsConfig(t, home, "win01", "win02")
	localID := "00000000-0000-0000-0000-000000000444"
	writeClaudeTranscript(t, home, localID, "local ok")
	if err := meta.Save(localID, meta.Meta{Name: "local-worker", Group: "run-fed"}); err != nil {
		t.Fatal(err)
	}
	writeLegacyLive(t, localID, "claude")

	stubRemoteReadOnce(t, func(_ context.Context, h config.Host, _ []string) ([]readRow, error) {
		if h.Name == "win01" {
			return nil, errors.New("offline")
		}
		return []readRow{{
			ID:    "remote-2",
			Name:  "remote-worker",
			Group: "run-fed",
			Turns: []session.NormTurn{{
				Seq: 5, Role: "assistant", Ts: "2026-07-02T00:00:01Z", Text: "remote ok",
				Tokens: session.NormTokens{Out: 1},
			}},
			Cursor: 5,
		}}, nil
	})

	var out string
	stderr := captureStderr(t, func() {
		out = captureStdout(t, func() {
			App{mux: inactiveMux{}}.Read([]string{"--run", "run-fed", "--active", "--hosts", "--limit", "1"})
		})
	})

	rows := decodeRowsForTest(t, out)
	byID := map[string]readRow{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	if local, ok := byID[localID]; !ok || local.Host != "local" {
		t.Fatalf("local row dropped when a host failed:\n%s", out)
	}
	if remote, ok := byID["win02/remote-2"]; !ok || remote.Host != "win02" {
		t.Fatalf("healthy host row dropped when another host failed: %+v", rows)
	}
	if _, ok := byID["win01/remote-2"]; ok {
		t.Fatalf("failing host contributed rows: %+v", rows)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want local + win02 only", rows)
	}
	if !strings.Contains(stderr, "win01") || !strings.Contains(stderr, "read failed") {
		t.Fatalf("failing host not reported on stderr: %q", stderr)
	}
}

// A federated follow must isolate a host whose stream fails: the error is
// reported on stderr and never aborts the aggregate, so local and healthy-host
// events keep flowing.
func TestReadFederatedFollowHostErrorIsolatedKeepsLocalAndOtherHosts(t *testing.T) {
	home := isolate(t)
	writeReadHostsConfig(t, home, "win01", "win02")
	localID := "00000000-0000-0000-0000-000000000555"
	writeClaudeTranscript(t, home, localID, "stale local")
	if err := meta.Save(localID, meta.Meta{Name: "local-worker", Group: "run-fed"}); err != nil {
		t.Fatal(err)
	}
	writeLegacyLive(t, localID, "claude")

	stubRemoteReadStream(t, func(ctx context.Context, h config.Host, _ []string, out chan<- federatedReadEvent) {
		if h.Name == "win01" {
			// Transport failure: report the error and contribute no events.
			select {
			case out <- federatedReadEvent{host: h.Name, err: errors.New("offline")}:
			case <-ctx.Done():
			}
			return
		}
		// Healthy host contributes one event, then nudges the local follow so the
		// aggregate proves both survive the failing host.
		select {
		case out <- federatedReadEvent{host: h.Name, event: follow.Event{
			ID: "remote-2", Name: "remote-worker", Group: "run-fed", Event: "output", Cursor: 4, Preview: "fresh remote",
		}}:
		case <-ctx.Done():
			return
		}
		time.Sleep(100 * time.Millisecond)
		appendFreshClaudeTurn(t, home, localID, "fresh local")
	})

	var out string
	stderr := captureStderr(t, func() {
		out = captureStdout(t, func() {
			App{mux: inactiveMux{}}.Read([]string{
				"--run", "run-fed", "--hosts", "--follow", "--active", "--from-now",
				"--events", "output", "--limit", "2", "--timeout", "3s",
			})
		})
	})

	if strings.Contains(out, "stale local") {
		t.Fatalf("from-now replayed stale data:\n%s", out)
	}
	events := decodeEventsForTest(t, out)
	if len(events) != 2 {
		t.Fatalf("events = %+v, want healthy remote + local despite a failing host", events)
	}
	seen := map[string]follow.Event{}
	for _, ev := range events {
		seen[ev.ID] = ev
	}
	if ev := seen["win02/remote-2"]; ev.Host != "win02" || ev.Preview != "fresh remote" {
		t.Fatalf("healthy host event dropped when another host failed: %+v", seen)
	}
	if ev := seen[localID]; ev.Host != "local" || ev.Preview != "fresh local" {
		t.Fatalf("local event dropped when a host failed: %+v", seen)
	}
	if !strings.Contains(stderr, "win01") || !strings.Contains(stderr, "read failed") {
		t.Fatalf("failing host not reported on stderr: %q", stderr)
	}
}
