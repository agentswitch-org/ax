package config

import (
	"os"
	"path/filepath"
	"testing"
)

func boolp(b bool) *bool { return &b }

// A built-in harness with no env override and no WSL boundary resolves to a
// single primary store from its default glob.
func TestResolveStoresDefaultSingle(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	cfg := Config{Harnesses: []Harness{{Name: "claude", Format: "claude", Glob: StringList{"~/.claude/projects/*/*.jsonl"}}}}
	ResolveStores(&cfg, nil)
	st := cfg.Harnesses[0].EffectiveStores()
	if len(st) != 1 {
		t.Fatalf("stores = %d, want 1: %+v", len(st), st)
	}
	if !st[0].Primary || st[0].Label != "" {
		t.Fatalf("primary/label wrong: %+v", st[0])
	}
	if want := ExpandHome("~/.claude/projects/*/*.jsonl"); st[0].Glob != want {
		t.Fatalf("glob = %q, want %q", st[0].Glob, want)
	}
}

// CLAUDE_CONFIG_DIR set to a non-default location makes that the primary store,
// with the built-in default kept as a secondary.
func TestResolveStoresEnvOverrideBecomesPrimary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	cfg := Config{Harnesses: []Harness{{Name: "claude", Format: "claude", Glob: StringList{"~/.claude/projects/*/*.jsonl"}}}}
	ResolveStores(&cfg, nil)
	st := cfg.Harnesses[0].EffectiveStores()
	if len(st) < 2 {
		t.Fatalf("want env store + default, got %+v", st)
	}
	wantPrimary := filepath.Join(dir, "projects", "*", "*.jsonl")
	if st[0].Glob != wantPrimary || !st[0].Primary || st[0].Source != "env:CLAUDE_CONFIG_DIR" {
		t.Fatalf("env store is not primary: %+v", st[0])
	}
}

// A user who names glob explicitly opts out of the env override entirely.
func TestResolveStoresUserGlobSkipsEnv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cfg := Config{Harnesses: []Harness{{Name: "claude", Format: "claude", Glob: StringList{"/explicit/*.jsonl"}}}}
	ResolveStores(&cfg, map[string]bool{"claude": true})
	st := cfg.Harnesses[0].EffectiveStores()
	if len(st) != 1 || st[0].Glob != "/explicit/*.jsonl" {
		t.Fatalf("explicit glob must win alone, got %+v", st)
	}
}

// auto_stores = false disables env and cross-boundary discovery.
func TestResolveStoresAutoOff(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cfg := Config{AutoStores: boolp(false), Harnesses: []Harness{{Name: "claude", Format: "claude", Glob: StringList{"~/.claude/projects/*/*.jsonl"}}}}
	ResolveStores(&cfg, nil)
	st := cfg.Harnesses[0].EffectiveStores()
	if len(st) != 1 || st[0].Source != "config" {
		t.Fatalf("auto off must leave only the config store, got %+v", st)
	}
}

// A list-valued glob yields one store per entry, first = primary.
func TestResolveStoresGlobList(t *testing.T) {
	cfg := Config{AutoStores: boolp(false), Harnesses: []Harness{{Name: "claude", Format: "claude", Glob: StringList{"/a/*.jsonl", "/b/*.jsonl"}}}}
	ResolveStores(&cfg, nil)
	st := cfg.Harnesses[0].EffectiveStores()
	if len(st) != 2 || !st[0].Primary || st[0].Glob != "/a/*.jsonl" || st[1].Glob != "/b/*.jsonl" {
		t.Fatalf("glob list not expanded in order: %+v", st)
	}
}

func TestWinToUnixWSL(t *testing.T) {
	cases := map[string]string{
		`C:\Users\noahi\src`: "/mnt/c/Users/noahi/src",
		`D:\work`:            "/mnt/d/work",
		`/home/agent/x`:      "/home/agent/x", // already POSIX: unchanged
		`relative\path`:      `relative\path`, // no drive letter: unchanged
	}
	for in, want := range cases {
		if got := winToUnixWSL(in, "/mnt/"); got != want {
			t.Errorf("winToUnixWSL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWinToUnixWSLCustomRoot(t *testing.T) {
	if got := winToUnixWSL(`C:\a`, "/windows/"); got != "/windows/c/a" {
		t.Fatalf("custom root = %q", got)
	}
}

func TestParseWSLList(t *testing.T) {
	// UTF-16LE "Ubuntu\nDebian\n" as the ASCII-with-NUL bytes wsl.exe emits.
	raw := []byte{}
	for _, r := range "Ubuntu\r\nDebian\r\n" {
		raw = append(raw, byte(r), 0)
	}
	got := parseWSLList(raw)
	if len(got) != 2 || got[0] != "Ubuntu" || got[1] != "Debian" {
		t.Fatalf("parseWSLList = %v", got)
	}
}

func TestWslMountRootDefault(t *testing.T) {
	// No /etc/wsl.conf on the test host (macOS/Linux CI): the default is /mnt/.
	if _, err := os.Stat("/etc/wsl.conf"); os.IsNotExist(err) {
		if got := wslMountRoot(); got != "/mnt/" {
			t.Fatalf("default mount root = %q, want /mnt/", got)
		}
	}
}

// windowsHomesUnder enumerates real user homes under a fake mount tree and skips
// service accounts, the discovery the WSL path relies on.
func TestWindowsHomesUnder(t *testing.T) {
	root := t.TempDir() + "/"
	for _, d := range []string{"c/Users/alice", "c/Users/Public", "c/Users/bob", "d/Users/carol"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{}
	for _, h := range windowsHomesUnder(root) {
		got[filepath.Base(h)] = true
	}
	if !got["alice"] || !got["bob"] || !got["carol"] {
		t.Fatalf("missing a real home: %v", got)
	}
	if got["Public"] {
		t.Fatalf("service account Public must be skipped: %v", got)
	}
}
