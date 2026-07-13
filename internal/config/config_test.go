package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/notify"
)

// An args-only override for a built-in must keep the built-in's glob/resume/
// launch, so a user can add flags without restating (and drifting from) the whole
// harness.
func TestLoadArgsOnlyOverrideKeepsBuiltin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[[harness]]
name = "claude"
args = "--dangerously-skip-permissions"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var claude *Harness
	for i := range cfg.Harnesses {
		if cfg.Harnesses[i].Name == "claude" {
			claude = &cfg.Harnesses[i]
		}
	}
	if claude == nil {
		t.Fatal("claude harness missing")
	}
	if claude.Args != "--dangerously-skip-permissions" {
		t.Fatalf("args = %q", claude.Args)
	}
	if claude.Glob == "" || claude.Resume == "" || !strings.Contains(claude.Launch, "{newid}") {
		t.Fatalf("built-in fields lost: glob=%q resume=%q launch=%q", claude.Glob, claude.Resume, claude.Launch)
	}
}

func TestRetentionConfigDefaultsAndOverrides(t *testing.T) {
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Retention.AutoRetire || cfg.Retention.RetainAfter != "10m" || !cfg.Retention.PruneCrashed ||
		!cfg.Retention.ReapConcludedWorkers || cfg.Retention.ReapAfter != "60s" || cfg.Retention.ReapDelay() != time.Minute {
		t.Fatalf("default retention = %+v", cfg.Retention)
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[retention]
auto_retire = false
retain_after = "30m"
prune_crashed = false
reap_concluded_workers = false
reap_after = "2m"
	`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retention.AutoRetire || cfg.Retention.RetainAfter != "30m" || cfg.Retention.PruneCrashed ||
		cfg.Retention.ReapConcludedWorkers || cfg.Retention.ReapAfter != "2m" || cfg.Retention.ReapDelay() != 2*time.Minute {
		t.Fatalf("overridden retention = %+v", cfg.Retention)
	}
}

func TestLocalComposeDirsLoadRawPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
behaviors_dir = "~/ax/behaviors"
recipes_dir = "C:\\Users\\noah\\ax\\recipes"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BehaviorsDir != "~/ax/behaviors" {
		t.Fatalf("BehaviorsDir = %q, want raw ~/ax/behaviors", cfg.BehaviorsDir)
	}
	if cfg.RecipesDir != `C:\Users\noah\ax\recipes` {
		t.Fatalf("RecipesDir = %q, want raw Windows path", cfg.RecipesDir)
	}
}

// With no explicit path set, behaviors_dir/recipes_dir default to a sibling of
// the config file, so content dropped next to the config is picked up with no
// config edit. This holds whether the config file is present-but-silent or
// absent entirely.
func TestContentDirsDefaultToConfigSibling(t *testing.T) {
	wantBehaviors := func() string { return filepath.Join(filepath.Dir(Path()), "behaviors") }
	wantRecipes := func() string { return filepath.Join(filepath.Dir(Path()), "recipes") }

	// Config file present but with no behaviors_dir/recipes_dir keys.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[[harness]]
name = "claude"
args = "--foo"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BehaviorsDir != wantBehaviors() {
		t.Fatalf("empty-config BehaviorsDir = %q, want sibling %q", cfg.BehaviorsDir, wantBehaviors())
	}
	if cfg.RecipesDir != wantRecipes() {
		t.Fatalf("empty-config RecipesDir = %q, want sibling %q", cfg.RecipesDir, wantRecipes())
	}

	// Config file absent entirely.
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BehaviorsDir != wantBehaviors() {
		t.Fatalf("absent-config BehaviorsDir = %q, want sibling %q", cfg.BehaviorsDir, wantBehaviors())
	}
	if cfg.RecipesDir != wantRecipes() {
		t.Fatalf("absent-config RecipesDir = %q, want sibling %q", cfg.RecipesDir, wantRecipes())
	}
}

// The built-in codex and pi templates must carry the flags their current CLIs
// (codex 0.142.x, pi 0.80.x) need to launch, resume, and run drivably. A drift
// here (a renamed/dropped flag, or losing the approval bypass) silently breaks
// the ax-driven interactive loop, so pin the load-bearing pieces.
func TestCodexAndPiBuiltinTemplates(t *testing.T) {
	byName := map[string]Harness{}
	for _, h := range Default().Harnesses {
		byName[h.Name] = h
	}

	codex := byName["codex"]
	// The interactive launch AND resume must both bypass per-command approval so
	// an `ax send` can drive the session to completion without a human.
	for _, tmpl := range []string{codex.Launch, codex.Resume} {
		if !strings.Contains(tmpl, "--sandbox danger-full-access") || !strings.Contains(tmpl, `approval_policy="never"`) {
			t.Fatalf("codex template missing sandbox/approval bypass: %q", tmpl)
		}
	}
	if !strings.Contains(codex.Resume, "codex resume {id}") {
		t.Fatalf("codex resume must address the session by id: %q", codex.Resume)
	}
	if !strings.Contains(codex.LaunchHeadless, "codex exec") || !strings.Contains(codex.LaunchHeadless, "--skip-git-repo-check") {
		t.Fatalf("codex headless must use `codex exec --skip-git-repo-check`: %q", codex.LaunchHeadless)
	}

	pi := byName["pi"]
	// pi mints an exact id from --session-id (so ax can hold/heartbeat it from the
	// start), resumes by --session, and carries the system prompt for {behavior}.
	if !strings.Contains(pi.Launch, "--session-id {newid}") || !strings.Contains(pi.Launch, "--append-system-prompt {behavior}") {
		t.Fatalf("pi launch drifted: %q", pi.Launch)
	}
	if !strings.Contains(pi.Resume, "pi --approve --session {id}") {
		t.Fatalf("pi resume drifted: %q", pi.Resume)
	}
	// --approve defuses pi's "Trust project folder?" modal, which otherwise
	// freezes an ax-driven session in any dir carrying `.pi/` resources. Every pi
	// invocation ax makes (interactive launch, headless, resume) must carry it.
	for _, tmpl := range []string{pi.Launch, pi.LaunchHeadless, pi.Resume} {
		if !strings.Contains(tmpl, "--approve") {
			t.Fatalf("pi template missing --approve project-trust bypass: %q", tmpl)
		}
	}
	if pi.Glob != "~/.pi/agent/sessions/*/*.jsonl" {
		t.Fatalf("pi glob drifted from the on-disk flat layout: %q", pi.Glob)
	}
	// The id-extracting regexes are load-bearing for indexing sessions off disk;
	// a drift here silently drops every session from the picker.
	if !strings.Contains(pi.IDRe, "?P<id>") {
		t.Fatalf("pi IDRe lost its id capture group: %q", pi.IDRe)
	}
	if codex.Glob != "~/.codex/sessions/*/*/*/rollout-*.jsonl" || !strings.Contains(codex.IDRe, "?P<id>") {
		t.Fatalf("codex glob/IDRe drifted: glob=%q idre=%q", codex.Glob, codex.IDRe)
	}
	// claude stays a plain human path: no sandbox/approval/trust flags leak into
	// its interactive templates.
	claude := byName["claude"]
	if strings.Contains(claude.Launch, "danger-full-access") || strings.Contains(claude.Launch, "--approve") {
		t.Fatalf("claude launch must stay unchanged: %q", claude.Launch)
	}
}

// notify = "bell" (string form) parses into Notify.Attention with Events nil.
func TestNotifyStringFormParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`notify = "bell"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Attention != "bell" {
		t.Fatalf("Attention = %q, want bell", cfg.Notify.Attention)
	}
	if cfg.Notify.Events != nil {
		t.Fatalf("Events should be nil in string form: %v", cfg.Notify.Events)
	}
}

// [notify] table form parses into Notify.Events with Attention empty.
func TestNotifyTableFormParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[notify]
run-success = "ax send {group} done"
needs-you   = "bell"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Attention != "" {
		t.Fatalf("Attention should be empty in table form: %q", cfg.Notify.Attention)
	}
	want := map[string]string{
		notify.RunSuccess: "ax send {group} done",
		notify.NeedsYou:   "bell",
	}
	for k, v := range want {
		if cfg.Notify.Events[k] != v {
			t.Fatalf("Events[%q] = %q, want %q", k, cfg.Notify.Events[k], v)
		}
	}
}

// metrics.textfile is off by default (empty string) and parses when set, so
// the run-conclusion textfile write stays opt-in.
func TestMetricsTextfileGate(t *testing.T) {
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Metrics.Textfile != "" {
		t.Fatalf("default Metrics.Textfile should be empty, got %q", cfg.Metrics.Textfile)
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[metrics]
textfile = "/var/lib/node_exporter/textfile_collector/ax.prom"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Metrics.Textfile != "/var/lib/node_exporter/textfile_collector/ax.prom" {
		t.Fatalf("Metrics.Textfile = %q", cfg.Metrics.Textfile)
	}
}

// [[bind]] entries are user-defined (no built-in defaults) and parse through
// Load like [[host]].
func TestBindTableParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[[bind]]
key = "e"
run = "${EDITOR:-vi} {transcript}"

[[bind]]
key  = "b"
run  = "${EDITOR:-vi} {file}"
file = "~/notes/backlog.md"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Binds) != 2 {
		t.Fatalf("Binds = %v, want 2 entries", cfg.Binds)
	}
	if cfg.Binds[0].Key != "e" || cfg.Binds[0].Run != "${EDITOR:-vi} {transcript}" {
		t.Fatalf("Binds[0] = %+v", cfg.Binds[0])
	}
	if cfg.Binds[1].File != "~/notes/backlog.md" {
		t.Fatalf("Binds[1].File = %q", cfg.Binds[1].File)
	}
}

// MuxPrefix is empty by default (mux backends resolve that to "ax:") and reads
// through from user config, including the "off" sentinel that disables
// namespacing entirely.
func TestMuxPrefixLoad(t *testing.T) {
	t.Setenv("AX_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MuxPrefix != "" {
		t.Fatalf("default MuxPrefix should be empty, got %q", cfg.MuxPrefix)
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`mux_prefix = "off"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MuxPrefix != "off" {
		t.Fatalf("MuxPrefix = %q, want off", cfg.MuxPrefix)
	}
}

func TestLoadExplicitBlankHarnessOverrideClearsBuiltin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[[harness]]
name = "claude"
waiting_re = ""
skip_permissions = ""
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var claude *Harness
	for i := range cfg.Harnesses {
		if cfg.Harnesses[i].Name == "claude" {
			claude = &cfg.Harnesses[i]
		}
	}
	if claude == nil {
		t.Fatal("claude harness missing")
	}
	if claude.WaitingRe != "" || claude.SkipPermissions != "" {
		t.Fatalf("explicit blank harness fields must clear built-ins, got waiting=%q skip=%q", claude.WaitingRe, claude.SkipPermissions)
	}
}

func TestLoadColumnDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[[column]]
key = "title"
width = 60

[[column]]
key = "tokens"
visible = false
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ColumnDefaults) != 2 {
		t.Fatalf("ColumnDefaults = %+v, want 2 entries", cfg.ColumnDefaults)
	}
	if cfg.ColumnDefaults[0].Key != "title" || cfg.ColumnDefaults[0].Width != 60 {
		t.Fatalf("first column default = %+v, want title/60", cfg.ColumnDefaults[0])
	}
	if cfg.ColumnDefaults[0].Visible != nil {
		t.Fatalf("title visible should be unset, got %v", *cfg.ColumnDefaults[0].Visible)
	}
	if cfg.ColumnDefaults[1].Key != "tokens" || cfg.ColumnDefaults[1].Visible == nil || *cfg.ColumnDefaults[1].Visible {
		t.Fatalf("second column default = %+v, want tokens/visible=false", cfg.ColumnDefaults[1])
	}
}
