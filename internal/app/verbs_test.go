package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/follow"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/session"
)

// GroupArg parses the current --run flag and the deprecated --group alias
// identically (see the group->run rename).
func TestGroupArgAcceptsRunAndDeprecatedGroup(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--run", "myrun", "x"}, "myrun"},
		{[]string{"--group", "myrun", "x"}, "myrun"}, // deprecated alias
		{[]string{"x"}, ""},
	}
	for _, c := range cases {
		got, rest := GroupArg(c.args)
		if got != c.want {
			t.Fatalf("GroupArg(%v) = %q, want %q", c.args, got, c.want)
		}
		if len(rest) != 1 || rest[0] != "x" {
			t.Fatalf("GroupArg(%v) rest = %v, want [x]", c.args, rest)
		}
	}
}

// ax tag --run (and the deprecated --group alias) both set the same metadata field.
func TestTagAcceptsRunAndDeprecatedGroup(t *testing.T) {
	var a App
	for _, flag := range []string{"--run", "--group"} {
		t.Run(flag, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			id := "sess-" + flag
			a.Tag([]string{id, flag, "myrun"})
			if got := meta.Load(id).Group; got != "myrun" {
				t.Fatalf("Tag with %s: meta.Group = %q, want %q", flag, got, "myrun")
			}
		})
	}
}

// `ax read <id>` resolves a full id exactly and an unambiguous short prefix to
// the one live local session it abbreviates; an ambiguous prefix reports the
// collision (caller errors, never fans out) and a non-matching prefix resolves
// to nothing (caller prints "no matching session"). Federated sessions
// (Host != "") never prefix-resolve.
func TestReadPrefixResolution(t *testing.T) {
	sessions := []session.Session{
		{ID: "abc123ff-0000-4000-8000-000000000001"},
		{ID: "abc9zz00-0000-4000-8000-000000000002"},
		{ID: "def45600-0000-4000-8000-000000000003"},
		{ID: "beef0000-0000-4000-8000-000000000004", Host: "win01"}, // federated
	}

	// exact full-id match still wins and is unambiguous by identity.
	if got := selectSessions(sessions, "def45600-0000-4000-8000-000000000003", ""); len(got) != 1 || got[0].ID != sessions[2].ID {
		t.Fatalf("exact match = %v, want the def session", got)
	}

	// unique short prefix resolves to exactly one session.
	if got := prefixMatches(sessions, "def456"); len(got) != 1 || got[0].ID != sessions[2].ID {
		t.Fatalf("unique prefix %q = %v, want the def session", "def456", got)
	}

	// ambiguous prefix reports every collision so the caller can error clearly.
	if got := prefixMatches(sessions, "abc"); len(got) != 2 {
		t.Fatalf("ambiguous prefix %q matched %d sessions, want 2", "abc", len(got))
	}

	// non-matching prefix resolves to nothing.
	if got := prefixMatches(sessions, "zzz"); len(got) != 0 {
		t.Fatalf("non-matching prefix %q matched %d sessions, want 0", "zzz", len(got))
	}

	// an empty id never prefix-resolves (a whole-index fan-out would be wrong).
	if got := prefixMatches(sessions, ""); got != nil {
		t.Fatalf("empty prefix matched %v, want nil", got)
	}

	// a federated session is not reachable by a bare-id prefix.
	if got := prefixMatches(sessions, "beef"); len(got) != 0 {
		t.Fatalf("prefix %q matched a federated session %v, want 0", "beef", got)
	}
}

func TestResolveIDFromSessionsAcceptsUniqueLocalPrefix(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	sessions := []session.Session{
		{ID: "abc123ff-0000-4000-8000-000000000001"},
		{ID: "abc9zz00-0000-4000-8000-000000000002"},
		{ID: "def45600-0000-4000-8000-000000000003"},
		{ID: "beef0000-0000-4000-8000-000000000004", Host: "win01"},
	}
	if got := resolveIDFromSessions("def456", sessions); got != sessions[2].ID {
		t.Fatalf("unique prefix resolved to %q, want %q", got, sessions[2].ID)
	}
	if got := resolveIDFromSessions("abc", sessions); got != "abc" {
		t.Fatalf("ambiguous prefix resolved to %q, want unchanged", got)
	}
	if got := resolveIDFromSessions("beef", sessions); got != "beef" {
		t.Fatalf("remote-only prefix resolved to %q, want unchanged", got)
	}
}

func TestResolveIDFromSessionsExactSessionBeatsStaleAlias(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const id = "00000000-0000-4000-8000-000000000101"
	const other = "00000000-0000-4000-8000-000000000202"
	if err := meta.SaveAlias(id, other); err != nil {
		t.Fatal(err)
	}
	got := resolveIDFromSessions(id, []session.Session{{ID: id}, {ID: other}})
	if got != id {
		t.Fatalf("exact local session resolved to %q, want %q", got, id)
	}
}

func TestReadActiveFiltersDormantAndConcludedSessions(t *testing.T) {
	sessions := []session.Session{
		{ID: "live", Group: "run-1"},
		{ID: "stale", Group: "run-1"},
		{ID: "dormant", Group: "run-1"},
	}
	snap := map[string]live.Entry{
		"live":  {Age: live.Fresh},
		"stale": {Age: live.Fresh + time.Second},
	}

	got := filterActive(filterGroup(sessions, "run-1"), snap)
	if len(got) != 1 || got[0].ID != "live" {
		t.Fatalf("active run members = %+v, want only live", got)
	}
}

func TestReadTargetsFromNowSeedsCursorAndWorkerName(t *testing.T) {
	path := t.TempDir() + "/worker.jsonl"
	line := `{"type":"assistant","timestamp":"2026-07-02T00:00:00Z","message":{"role":"assistant","content":"historical","usage":{"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	cfg := config.Config{Harnesses: []config.Harness{{Name: "claude", Format: "claude"}}}
	s := session.Session{ID: "worker-1", Name: "builder", Group: "run-1", Harness: "claude", File: path}
	base := startCursors(cfg, []session.Session{s})

	got := readTargets(cfg, []session.Session{s}, readOpts{fromNow: true}, base, time.Now())
	if len(got) != 1 {
		t.Fatalf("readTargets returned %d targets, want 1", len(got))
	}
	if got[0].Name != "builder" || got[0].Group != "run-1" {
		t.Fatalf("target identity = %+v, want worker name and run", got[0])
	}
	if !got[0].UseStartCursor || got[0].StartCursor != 1 {
		t.Fatalf("start cursor = %+v, want explicit cursor 1 to skip history", got[0])
	}
}

func TestRunReadExcludeSkipsSelfButKeepsWorkerText(t *testing.T) {
	home := isolate(t)
	selfID := "00000000-0000-0000-0000-00000000c0de"
	workerID := "00000000-0000-0000-0000-00000000feed"
	writeClaudeTranscript(t, home, selfID, "self progress update")
	writeClaudeTranscript(t, home, workerID, "worker final result")
	if err := meta.Save(selfID, meta.Meta{Group: "run-1", Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := meta.Save(workerID, meta.Meta{Group: "run-1", Harness: "claude", Name: "worker"}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Read([]string{"--run", "run-1", "--exclude", selfID, "--format", "text"})
	})
	if strings.Contains(out, "self progress update") {
		t.Fatalf("excluded self turn leaked into text output:\n%s", out)
	}
	if !strings.Contains(out, "worker final result") {
		t.Fatalf("worker final turn missing from text output:\n%s", out)
	}
}

func TestRunReadFollowExcludeFromNowSkipsSelfAndEmitsWorker(t *testing.T) {
	home := isolate(t)
	selfID := "00000000-0000-0000-0000-00000000c0de"
	workerID := "00000000-0000-0000-0000-00000000feed"
	writeClaudeTranscript(t, home, selfID, "old self turn")
	writeClaudeTranscript(t, home, workerID, "old worker turn")
	if err := meta.Save(selfID, meta.Meta{Group: "run-1", Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := meta.Save(workerID, meta.Meta{Group: "run-1", Harness: "claude", Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	writeLegacyLive(t, selfID, "ax run "+selfID)
	writeLegacyLive(t, workerID, "ax run "+workerID)

	done := make(chan string, 1)
	go func() {
		done <- captureStdout(t, func() {
			App{mux: readTestMux{}}.Read([]string{
				"--run", "run-1",
				"--follow",
				"--active",
				"--from-now",
				"--limit", "1",
				"--timeout", "4s",
				"--exclude", selfID,
			})
		})
	}()
	time.Sleep(500 * time.Millisecond)
	appendClaudeAssistantTurn(t, claudeTranscriptPath(home, selfID), "fresh self progress")
	appendClaudeAssistantTurn(t, claudeTranscriptPath(home, workerID), "fresh worker final result")

	var out string
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("read --follow did not return after worker event")
	}
	if strings.Contains(out, "fresh self progress") {
		t.Fatalf("excluded self event leaked into follow output:\n%s", out)
	}
	var ev follow.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ev); err != nil {
		t.Fatalf("decode follow event %q: %v", out, err)
	}
	if ev.ID != workerID || ev.Event != "turn" || !strings.Contains(ev.Preview, "fresh worker final result") {
		t.Fatalf("follow event = %+v, want worker turn with final preview; raw:\n%s", ev, out)
	}
}

type readTestMux struct{ inactiveMux }

func (readTestMux) PaneTail(string, int) string { return "" }

func TestDirectReadIgnoresRunExcludeFilter(t *testing.T) {
	home := isolate(t)
	id := "00000000-0000-0000-0000-00000000c0de"
	writeClaudeTranscript(t, home, id, "direct read remains visible")
	if err := meta.Save(id, meta.Meta{Group: "run-1", Harness: "claude"}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Read([]string{id, "--exclude", id, "--format", "text"})
	})
	if !strings.Contains(out, "direct read remains visible") {
		t.Fatalf("direct read should ignore run exclusion filter, got:\n%s", out)
	}
}

func claudeTranscriptPath(home, id string) string {
	return filepath.Join(home, ".claude", "projects", "proj", id+".jsonl")
}

func appendClaudeAssistantTurn(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	line := `{"type":"assistant","timestamp":"2026-07-02T00:00:02Z","message":{"role":"assistant","content":` +
		mustJSON(text) + `,"usage":{"output_tokens":1}}}` + "\n"
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		t.Fatalf("write transcript: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close transcript: %v", err)
	}
	old := time.Now().Add(-live.Active - time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("backdate transcript: %v", err)
	}
}

func TestCascadeKillOrdersRecipeRootLast(t *testing.T) {
	root := session.Session{ID: "recipe-root", Harness: "recipe", Mode: "recipe", Group: "run-1"}
	child := session.Session{ID: "child-1", Parent: root.ID, Group: "run-1"}
	grandchild := session.Session{ID: "grandchild-1", Parent: child.ID, Group: "run-1"}
	members := []session.Session{root, child, grandchild}
	byID := map[string]session.Session{root.ID: root, child.ID: child, grandchild.ID: grandchild}

	sortSessionsDeepestFirst(members, byID)

	got := []string{members[0].ID, members[1].ID, members[2].ID}
	want := []string{"grandchild-1", "child-1", "recipe-root"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kill order = %#v, want %#v", got, want)
	}
}
