package app

import (
	"path/filepath"
	"strings"
	"testing"
)

// The write fence is opt-in via --write / --no-write and nothing else: no
// behavior text infers it, and --write and --no-write are mutually exclusive.
func TestWriteFenceIsOptInViaFlag(t *testing.T) {
	// --write engages the fence with a write scope (repeatable).
	o, err := parseLaunch([]string{"task", "--write", "./out/**/*.md", "--write", "./notes.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.fen.writeGlobs) != 2 || o.fen.writeGlobs[0] != "./out/**/*.md" || o.fen.writeGlobs[1] != "./notes.md" {
		t.Fatalf("--write must append into the write scope, got %v", o.fen.writeGlobs)
	}
	if o.fen.noWrite {
		t.Fatal("--write must not set noWrite")
	}

	// --no-write engages the fence with an empty write scope.
	o, err = parseLaunch([]string{"task", "--no-write"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.fen.noWrite || len(o.fen.writeGlobs) != 0 {
		t.Fatalf("--no-write must set noWrite with no globs, got noWrite=%v globs=%v", o.fen.noWrite, o.fen.writeGlobs)
	}

	// --no-write and --write together is a usage error.
	if _, err = parseLaunch([]string{"task", "--no-write", "--write", "./x.md"}); err == nil {
		t.Fatal("--no-write with --write must be a usage error")
	}

	// With no fence flag, even a --behavior launch stays unfenced (writable): the
	// old behavior-keyed default really is gone.
	o, err = parseLaunch([]string{"task", "--behavior", "some-behavior"})
	if err != nil {
		t.Fatal(err)
	}
	if o.fen.noWrite || len(o.fen.writeGlobs) > 0 {
		t.Fatal("a --behavior launch must not be fenced by default")
	}
}

// fenceDecide is the pure per-tool decision. --no-subagents bars the sub-agent
// spawn tools (they must delegate via `ax claude ...`); without it they are
// allowed. Read stays allowed and a Write outside the (empty) scope stays denied
// either way.
func TestFenceDecideNoSubagents(t *testing.T) {
	cases := []struct {
		tool        string
		noSubagents bool
		allow       bool
	}{
		{"Task", true, false},
		{"TaskCreate", true, false},
		{"Agent", true, false},
		{"Task", false, true},
		{"TaskCreate", false, true},
		{"Agent", false, true},
		{"Read", true, true},
		{"Read", false, true},
		{"Write", true, false},
		{"Write", false, false},
	}
	for _, c := range cases {
		d := fenceDecide(c.tool, "", "/tmp/x.md", nil, "/wd", c.noSubagents)
		if d.Allow != c.allow {
			t.Errorf("fenceDecide(%q, noSubagents=%v).Allow = %v, want %v (reason=%q)", c.tool, c.noSubagents, d.Allow, c.allow, d.Reason)
		}
	}
}

// fenceDecide gates a file-write by the write scope: a target matching a glob is
// allowed, a non-matching one is denied, and an empty scope (--no-write) denies
// every write.
func TestFenceDecideWriteScope(t *testing.T) {
	// Build the scope off a native-absolute root so the paths are absolute on
	// every OS (a hardcoded "/wd" is not absolute on Windows, where filepath.Join
	// would then double it against the cwd and the match would spuriously fail).
	wd := filepath.Join(t.TempDir(), "wd")
	globs := []string{filepath.Join(wd, "out", "**", "*.md")}
	if d := fenceDecide("Write", "", filepath.Join(wd, "out", "sub", "state.md"), globs, wd, false); !d.Allow {
		t.Errorf("a write matching the scope must be allowed (reason=%q)", d.Reason)
	}
	if d := fenceDecide("Edit", "", filepath.Join(wd, "src", "main.go"), globs, wd, false); d.Allow {
		t.Error("a write outside the scope must be denied")
	}
	// A relative target resolves against wd, then matches.
	if d := fenceDecide("Write", "", filepath.Join("out", "notes.md"), globs, wd, false); !d.Allow {
		t.Errorf("a relative write matching the scope must be allowed (reason=%q)", d.Reason)
	}
	// Empty scope (--no-write): every write denied.
	if d := fenceDecide("Write", "", filepath.Join(wd, "out", "state.md"), nil, wd, false); d.Allow {
		t.Error("--no-write must deny every write")
	}
}

// --no-subagents parses into the fence as a bool capability, orthogonal to the
// write scope.
func TestNoSubagentsFlagParses(t *testing.T) {
	o, err := parseLaunch([]string{"task", "--no-write", "--no-subagents"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.fen.noSubagents {
		t.Fatal("--no-subagents must set the capability")
	}
	// Absent: off.
	o, err = parseLaunch([]string{"task", "--no-write"})
	if err != nil {
		t.Fatal(err)
	}
	if o.fen.noSubagents {
		t.Fatal("--no-subagents must default off")
	}
}

// AX_NO_SUBAGENTS is injected UNCONDITIONALLY so the capability never inherits: 1
// when set, empty (shadowing any leaked value) when not.
func TestNoSubagentsShadowedInEnv(t *testing.T) {
	env := axEnv("id", "grp", "parent", 0, 1, fences{noWrite: true, noSubagents: true}, nil)
	if !hasEnv(env, "AX_NO_SUBAGENTS=1") {
		t.Fatalf("AX_NO_SUBAGENTS=1 must be injected when set, got %v", env)
	}
	env = axEnv("id", "grp", "parent", 0, 1, fences{noWrite: true}, nil)
	if !hasEnv(env, "AX_NO_SUBAGENTS=") {
		t.Fatalf("AX_NO_SUBAGENTS= (empty) must shadow when unset, got %v", env)
	}
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// claude is fenceable: the args carry the --settings blob and the bypass flag is
// stripped (a fence with a permission bypass is no fence).
func TestFenceHargsClaude(t *testing.T) {
	out, refuse, warn := fenceHargs("claude", "--dangerously-skip-permissions --foo", false, nil, false)
	if refuse {
		t.Fatal("claude must be fenceable, not refused")
	}
	if warn != "" {
		t.Fatalf("claude fence should not warn, got %q", warn)
	}
	if strings.Contains(out, "dangerously") {
		t.Fatalf("bypass flag must be stripped when fencing, got %q", out)
	}
	if !strings.Contains(out, "--settings") || !strings.Contains(out, "fence-check") {
		t.Fatalf("fenced args must install --settings with the fence-check hook, got %q", out)
	}
	if !strings.Contains(out, "--foo") {
		t.Fatalf("non-bypass args must survive, got %q", out)
	}
}

// codex/pi/opencode cannot be fenced: without best-effort the launch is refused.
func TestFenceHargsUnsupportedRefused(t *testing.T) {
	for _, f := range []string{"codex", "pi", "opencode"} {
		_, refuse, _ := fenceHargs(f, "", false, nil, false)
		if !refuse {
			t.Errorf("harness %q must be refused when it cannot be fenced", f)
		}
	}
}

// --fence best-effort downgrades an un-fenceable harness to an unfenced launch
// with a warning instead of refusing.
func TestFenceHargsBestEffortWarns(t *testing.T) {
	_, refuse, warn := fenceHargs("codex", "", true, nil, false)
	if refuse {
		t.Fatal("best-effort must not refuse")
	}
	if warn == "" {
		t.Fatal("best-effort must warn that the launch is unfenced")
	}
}

// --write parses into the write scope and, with a scope present, fenceHargs drops
// the file-write tools from the settings deny list so the hook can gate them by
// path, while every other mutating tool stays denied. --no-write keeps them denied.
func TestWriteFlagAndDenyList(t *testing.T) {
	o, err := parseLaunch([]string{"task", "--write", "./out/**/*.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.fen.writeGlobs) != 1 || o.fen.writeGlobs[0] != "./out/**/*.md" {
		t.Fatalf("--write must parse into the write scope, got %v", o.fen.writeGlobs)
	}

	// --no-write (empty scope): the write tools are hard-denied in the settings blob.
	noWrite, _, _ := fenceHargs("claude", "", false, nil, false)
	if !strings.Contains(noWrite, "Write") || !strings.Contains(noWrite, "Edit") {
		t.Fatalf("--no-write fence must deny Write/Edit in settings, got %q", noWrite)
	}

	// With a write scope, Write/Edit/MultiEdit leave the deny list (the hook gates
	// them), but NotebookEdit and mcp__* stay denied.
	out, _, _ := fenceHargs("claude", "", false, []string{"/wd/out/**/*.md"}, false)
	for _, tool := range []string{"\"Write\"", "\"Edit\"", "\"MultiEdit\""} {
		if strings.Contains(out, tool) {
			t.Fatalf("write-scope fence must drop %s from the deny list, got %q", tool, out)
		}
	}
	for _, tool := range []string{"NotebookEdit", "mcp__*"} {
		if !strings.Contains(out, tool) {
			t.Fatalf("write-scope fence must still deny %s, got %q", tool, out)
		}
	}

	// Without --no-subagents the sub-agent spawn tools are NOT in the deny list;
	// with it they are (a role-agnostic backstop, here alongside a write scope).
	if strings.Contains(out, "Task") {
		t.Fatalf("fence without --no-subagents must not deny Task, got %q", out)
	}
	noSub, _, _ := fenceHargs("claude", "", false, []string{"/wd/out/**/*.md"}, true)
	for _, tool := range []string{"Task", "TaskCreate", "Agent"} {
		if !strings.Contains(noSub, tool) {
			t.Fatalf("--no-subagents fence must deny %s, got %q", tool, noSub)
		}
	}
}

// AX_WRITE is injected whenever the launch is fenced, so the child's fence-check
// hook learns the write scope: the newline-joined globs for --write, an empty
// value for --no-write (which also shadows any leaked parent AX_WRITE). A
// non-fenced (writable) launch leaves AX_WRITE unset.
func TestWriteInjectedIntoEnv(t *testing.T) {
	// --write: newline-joined globs.
	env := axEnv("id", "grp", "parent", 0, 1, fences{writeGlobs: []string{"/wd/out/**/*.md", "/wd/notes.md"}}, nil)
	if !hasEnv(env, "AX_WRITE=/wd/out/**/*.md\n/wd/notes.md") {
		t.Fatalf("AX_WRITE must carry the newline-joined globs, got %v", env)
	}

	// --no-write: AX_WRITE set to empty (present so it shadows an inherited value).
	env = axEnv("id", "grp", "parent", 0, 1, fences{noWrite: true}, nil)
	if !hasEnv(env, "AX_WRITE=") {
		t.Fatalf("AX_WRITE= (empty) must be injected for --no-write, got %v", env)
	}

	// Non-fenced (writable) launch: no AX_WRITE at all, so a writable child is not
	// silently fenced by an inherited value the launcher never re-set.
	env = axEnv("id", "grp", "parent", 0, 1, fences{}, nil)
	for _, e := range env {
		if strings.HasPrefix(e, "AX_WRITE=") {
			t.Fatalf("AX_WRITE must not be set for a non-fenced launch, got %q", e)
		}
	}
}

// The fence is per-session: neither the old read-only env nor a scratch dir is
// injected, so those legacy variables never appear.
func TestNoLegacyFenceEnv(t *testing.T) {
	env := axEnv("id", "grp", "parent", 0, 1, fences{noWrite: true, maxCost: 5}, nil)
	for _, e := range env {
		if strings.Contains(e, "READONLY") || strings.Contains(e, "READ_ONLY") || strings.HasPrefix(e, "AX_SCRATCH=") || strings.HasPrefix(e, "AX_ROLE=") {
			t.Fatalf("legacy fence env must not be injected, found %q", e)
		}
	}
}
