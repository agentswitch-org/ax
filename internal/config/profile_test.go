package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProfileExtractsPortableFieldsOnly asserts Profile() returns exactly the
// portable fields and none of the local ones (Glob/DB stay behind; Hosts, Shell,
// Mux and friends are not part of a Profile's type at all).
func TestProfileExtractsPortableFieldsOnly(t *testing.T) {
	c := Config{
		Harnesses: []Harness{{
			Name: "claude", Glob: "/local/*.jsonl", DB: "~/x.db",
			Launch: "L", Args: "A", Format: "claude", SkipPermissions: "S",
		}},
		Columns:        []string{"host", "harness"},
		MuxPrefix:      "p:",
		DefaultHarness: "claude",
		// Local fields that must not surface in the profile.
		Shell: "zsh -lic", Mux: "zellij", HoldBackend: "dtach", Offline: true,
		BehaviorsDir: "~/ax/behaviors",
		RecipesDir:   "~/ax/recipes",
		Hosts:        []Host{{Name: "win01", Transport: "ssh win01"}},
		Retention:    Retention{AutoRetire: false, RetainAfter: "99h", PruneCrashed: false},
	}
	p := c.Profile()
	if len(p.Harnesses) != 1 {
		t.Fatalf("expected 1 harness in profile, got %d", len(p.Harnesses))
	}
	h := p.Harnesses[0]
	if h.Launch != "L" || h.Args != "A" || h.Format != "claude" || h.SkipPermissions != "S" {
		t.Fatalf("portable harness fields not carried: %+v", h)
	}
	// A HarnessProfile has no Glob/DB field, so the machine paths cannot leak by
	// construction; re-encode and confirm the paths are absent from the TOML too.
	data, err := EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, leak := range []string{"/local/", "x.db", "zsh -lic", "zellij", "dtach", "win01", "~/ax/behaviors", "~/ax/recipes", "offline", "99h", "retention"} {
		if strings.Contains(s, leak) {
			t.Fatalf("local value %q leaked into profile TOML:\n%s", leak, s)
		}
	}
	if p.MuxPrefix != "p:" || p.DefaultHarness != "claude" {
		t.Fatalf("UI fields not carried: %+v", p)
	}
}

func TestProfileExcludesLocalComposeDirs(t *testing.T) {
	cfg := Config{
		BehaviorsDir: "~/ax/behaviors",
		RecipesDir:   "~/ax/recipes",
		Harnesses: []Harness{{
			Name:   "claude",
			Format: "claude",
			Launch: "claude {task}",
		}},
	}

	data, err := EncodeProfile(cfg.Profile())
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, key := range []string{"behaviors_dir", "recipes_dir"} {
		if strings.Contains(s, key) {
			t.Fatalf("local compose path key %q leaked into profile TOML:\n%s", key, s)
		}
	}
}

// writeCfg points AX_CONFIG at a temp file with body and returns its path.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)
	return path
}

// TestApplyMergesTemplatesPreservesLocal drives a full apply: the incoming
// profile overwrites the target harness's template fields by name while keeping
// its local Glob/DB, replaces the UI fields, and leaves every LOCAL field
// (notify, mux, host) untouched. It also asserts the backup and atomic write.
func TestApplyMergesTemplatesPreservesLocal(t *testing.T) {
	path := writeCfg(t, `notify = "bell"
mux = "zellij"
default_harness = "claude"

[[host]]
name = "otherbox"
transport = "ssh otherbox"

[[harness]]
name = "claude"
glob = "/custom/local/*.jsonl"
db = "~/recv.db"
args = "--recv-args"
launch = "old-launch {task}"
`)

	inc := Profile{
		DefaultHarness: "codex",
		MuxPrefix:      "sender:",
		Harnesses: []HarnessProfile{{
			Name: "claude", Launch: "new-launch {task}", Args: "--sender-args", Format: "claude",
		}},
	}

	backup, err := ApplyProfileToFile(inc, 12345)
	if err != nil {
		t.Fatal(err)
	}
	if backup != path+".bak.12345" {
		t.Fatalf("backup path = %q, want %s.bak.12345", backup, path)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("backup not written: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// LOCAL preserved.
	if got.Mux != "zellij" {
		t.Fatalf("local mux clobbered: %q", got.Mux)
	}
	if got.Notify.Attention != "bell" {
		t.Fatalf("local notify clobbered: %+v (custom-unmarshaler round-trip broke)", got.Notify)
	}
	if len(got.Hosts) != 1 || got.Hosts[0].Name != "otherbox" {
		t.Fatalf("local hosts clobbered: %+v", got.Hosts)
	}
	// UI replaced.
	if got.DefaultHarness != "codex" || got.MuxPrefix != "sender:" {
		t.Fatalf("UI fields not applied: default=%q muxprefix=%q", got.DefaultHarness, got.MuxPrefix)
	}
	// Harness template overwritten, local Glob/DB preserved.
	var claude *Harness
	for i := range got.Harnesses {
		if got.Harnesses[i].Name == "claude" {
			claude = &got.Harnesses[i]
		}
	}
	if claude == nil {
		t.Fatal("claude harness vanished")
	}
	if claude.Launch != "new-launch {task}" || claude.Args != "--sender-args" {
		t.Fatalf("template not overwritten: launch=%q args=%q", claude.Launch, claude.Args)
	}
	if claude.Glob != "/custom/local/*.jsonl" || claude.DB != "~/recv.db" {
		t.Fatalf("local glob/db not preserved: glob=%q db=%q", claude.Glob, claude.DB)
	}
}

func TestApplyClearsExplicitBlankHarnessFields(t *testing.T) {
	writeCfg(t, `[[harness]]
name = "custom"
glob = "/custom/*.jsonl"
db = "~/custom.db"
format = "old-format"
launch = "old-launch"
args = "--stale"
waiting_re = "stale prompt"
skip_permissions = "--stale-skip"
`)

	inc, err := DecodeProfile([]byte(`[[harness]]
name = "custom"
format = "new-format"
launch = "new-launch"
args = ""
waiting_re = ""
skip_permissions = ""
`))
	if err != nil {
		t.Fatal(err)
	}
	if diff := DiffProfile(mustLoad(t).Profile(), inc); len(diff) == 0 {
		t.Fatal("explicit blank incoming fields should be reported as changes before apply")
	}
	if _, err := ApplyProfileToFile(inc, 222); err != nil {
		t.Fatal(err)
	}

	got := mustLoad(t)
	var custom *Harness
	for i := range got.Harnesses {
		if got.Harnesses[i].Name == "custom" {
			custom = &got.Harnesses[i]
		}
	}
	if custom == nil {
		t.Fatal("custom harness vanished")
	}
	if custom.Args != "" || custom.WaitingRe != "" || custom.SkipPermissions != "" {
		t.Fatalf("blank profile fields did not clear stale target values: args=%q waiting=%q skip=%q", custom.Args, custom.WaitingRe, custom.SkipPermissions)
	}
	if custom.Format != "new-format" || custom.Launch != "new-launch" {
		t.Fatalf("non-empty profile fields not applied: format=%q launch=%q", custom.Format, custom.Launch)
	}
	if custom.Glob != "/custom/*.jsonl" || custom.DB != "~/custom.db" {
		t.Fatalf("local glob/db not preserved: glob=%q db=%q", custom.Glob, custom.DB)
	}
	if diff := DiffProfile(got.Profile(), inc); len(diff) != 0 {
		t.Fatalf("after apply explicit blank profile should be in sync, diff = %v", diff)
	}

	exported, err := EncodeProfile(Config{Harnesses: []Harness{{Name: "custom", Format: "new-format"}}}.Profile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exported), `args = ""`) {
		t.Fatalf("exported profiles must carry blank harness fields explicitly:\n%s", exported)
	}
}

func TestApplyUpdatesDuplicateHarnessTablesThatLoadWouldWin(t *testing.T) {
	path := writeCfg(t, `[[harness]]
name = "custom"
glob = "/first/*.jsonl"
args = "--first-stale"

[[harness]]
name = "custom"
glob = "/second/*.jsonl"
args = "--second-stale"
`)
	inc := Profile{Harnesses: []HarnessProfile{{Name: "custom", Format: "custom", Args: "--applied"}}}

	if _, err := ApplyProfileToFile(inc, 333); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), `args = "--applied"`) != 2 {
		t.Fatalf("all duplicate target harness tables should be updated:\n%s", data)
	}
	if !strings.Contains(string(data), `glob = "/first/*.jsonl"`) || !strings.Contains(string(data), `glob = "/second/*.jsonl"`) {
		t.Fatalf("duplicate harness local globs should be preserved:\n%s", data)
	}

	got := mustLoad(t)
	var custom *Harness
	for i := range got.Harnesses {
		if got.Harnesses[i].Name == "custom" {
			custom = &got.Harnesses[i]
		}
	}
	if custom == nil {
		t.Fatal("custom harness vanished")
	}
	if custom.Args != "--applied" {
		t.Fatalf("later duplicate still won with stale args: %+v", custom)
	}
	if diff := DiffProfile(got.Profile(), inc); len(diff) != 0 {
		t.Fatalf("duplicate harness apply should converge, diff = %v", diff)
	}
}

// TestApplyIsIdempotent applies a profile, then applies the same profile again;
// the second time is a no-op (diff empty) so nothing more is written.
func TestApplyIsIdempotent(t *testing.T) {
	writeCfg(t, `[[harness]]
name = "claude"
glob = "/local/*.jsonl"
args = "--x"
`)
	inc := Profile{Harnesses: []HarnessProfile{{Name: "claude", Args: "--y", Launch: "L"}}}

	if _, err := ApplyProfileToFile(inc, 1); err != nil {
		t.Fatal(err)
	}
	cur, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if d := DiffProfile(cur.Profile(), inc); len(d) != 0 {
		t.Fatalf("after apply the profile should be in sync, diff = %v", d)
	}
}

// TestLintRejectsSecretsAndHomePaths asserts the sender-side lint fails a
// profile carrying a secret or an absolute home path, naming the field, and
// accepts a clean profile.
func TestLintRejectsSecretsAndHomePaths(t *testing.T) {
	cases := []struct {
		name  string
		p     Profile
		field string
	}{
		{"api key", Profile{Harnesses: []HarnessProfile{{Name: "c", Args: "--api-key sk-abc"}}}, "harness[c].args"},
		{"bearer", Profile{Harnesses: []HarnessProfile{{Name: "c", Launch: "curl -H 'Bearer t' {task}"}}}, "harness[c].launch"},
		{"home path", Profile{Harnesses: []HarnessProfile{{Name: "c", Resume: "cd /Users/bob {id}"}}}, "harness[c].resume"},
		{"harness name", Profile{Harnesses: []HarnessProfile{{Name: "/Users/bob/h"}}}, "harness.name"},
		{"column key", Profile{ColumnDefaults: []ColumnDefault{{Key: "/home/bob/title"}}}, "column[0].key"},
		{"keys action", Profile{Keys: map[string]StringList{"api_key": {"x"}}}, "keys.api_key"},
		{"ui home path", Profile{MuxPrefix: "/home/bob/"}, "mux_prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := LintProfile(tc.p)
			if err == nil {
				t.Fatalf("lint accepted a dirty profile")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("lint error should name %q, got: %v", tc.field, err)
			}
		})
	}
	clean := Profile{
		DefaultHarness: "claude",
		Harnesses: []HarnessProfile{{
			Name: "claude", Launch: "claude --session-id {newid} {args} {task}",
			SkipPermissions: "--dangerously-skip-permissions",
		}},
	}
	if err := LintProfile(clean); err != nil {
		t.Fatalf("lint rejected a clean profile: %v", err)
	}
}

func TestApplyBackupUsesUniqueSuffix(t *testing.T) {
	path := writeCfg(t, `default_harness = "pi"`+"\n")

	first, err := ApplyProfileToFile(Profile{MuxPrefix: "one:"}, 444)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ApplyProfileToFile(Profile{MuxPrefix: "two:"}, 444)
	if err != nil {
		t.Fatal(err)
	}
	if first != path+".bak.444" || second != path+".bak.445" {
		t.Fatalf("backup paths should use unique numeric suffixes, got %q and %q", first, second)
	}
	firstData, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(firstData), `default_harness = "pi"`) {
		t.Fatalf("first backup lost the original config:\n%s", firstData)
	}
	if !strings.Contains(string(secondData), `mux_prefix = "one:"`) {
		t.Fatalf("second backup should snapshot the config before the second apply:\n%s", secondData)
	}
	found, _, ok := LatestBackup()
	if !ok || found != second {
		t.Fatalf("latest backup should be the second unique suffix, got %q ok=%v", found, ok)
	}
}

// TestApplyToFreshFileWritesNewConfig covers a target with no existing config:
// the profile is written and no backup is produced.
func TestApplyToFreshFileWritesNewConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.toml") // dir does not exist yet
	t.Setenv("AX_CONFIG", path)
	inc := Profile{DefaultHarness: "codex", Harnesses: []HarnessProfile{{Name: "codex", Launch: "L"}}}
	backup, err := ApplyProfileToFile(inc, 7)
	if err != nil {
		t.Fatal(err)
	}
	if backup != "" {
		t.Fatalf("no prior file should mean no backup, got %q", backup)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

func mustLoad(t *testing.T) Config {
	t.Helper()
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
