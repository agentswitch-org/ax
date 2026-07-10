package app

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/session"
)

// --task-file reads the file content as the task.
func TestParseLaunchTaskFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "task*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("do the thing\nand more stuff"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	o, err := parseLaunch([]string{"--task-file", f.Name(), "--model", "sonnet"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.task != "do the thing\nand more stuff" {
		t.Fatalf("task = %q, want multi-line file content", o.task)
	}
	if o.model != "sonnet" {
		t.Fatalf("other flags not parsed: model = %q", o.model)
	}
}

// --task-file - reads from stdin.
func TestParseLaunchTaskFileStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig; r.Close() }()

	if _, err := w.Write([]byte("task from stdin\nline 2")); err != nil {
		t.Fatal(err)
	}
	w.Close()

	o, err := parseLaunch([]string{"--task-file", "-"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.task != "task from stdin\nline 2" {
		t.Fatalf("task = %q", o.task)
	}
}

// Positional - reads from stdin (existing mechanism).
func TestParseLaunchStdinPositional(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig; r.Close() }()

	if _, err := w.Write([]byte("task via positional stdin")); err != nil {
		t.Fatal(err)
	}
	w.Close()

	o, err := parseLaunch([]string{"-"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.task != "task via positional stdin" {
		t.Fatalf("task = %q", o.task)
	}
}

// Positional task still works unchanged.
func TestParseLaunchPositionalTaskUnchanged(t *testing.T) {
	o, err := parseLaunch([]string{"do the thing", "--model", "opus"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.task != "do the thing" || o.model != "opus" {
		t.Fatalf("task=%q model=%q", o.task, o.model)
	}
}

// --task-file and a positional task together must error.
func TestParseLaunchTaskFileConflict(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "task*.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := parseLaunch([]string{"positional task", "--task-file", f.Name()}); err == nil {
		t.Fatal("--task-file with a positional task must error")
	}
	// stdin positional + --task-file also conflicts.
	if _, err := parseLaunch([]string{"-", "--task-file", f.Name()}); err == nil {
		t.Fatal("--task-file with '-' positional must error")
	}
}

// --task-file with a nonexistent path must error.
func TestParseLaunchTaskFileMissing(t *testing.T) {
	if _, err := parseLaunch([]string{"--task-file", "/no/such/file.txt"}); err == nil {
		t.Fatal("--task-file with a missing file must error")
	}
}

func TestParseLaunchBehaviorText(t *testing.T) {
	o, err := parseLaunch([]string{"task", "--behavior-text", "inline\nbehavior"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.behaviorText != "inline\nbehavior" {
		t.Fatalf("behaviorText = %q", o.behaviorText)
	}
}

func TestParseLaunchBehaviorMutualExclusion(t *testing.T) {
	if _, err := parseLaunch([]string{"task", "--behavior", "behavior.md", "--behavior-text", "inline"}); err == nil {
		t.Fatal("--behavior and --behavior-text together must error")
	}
}

func TestRunWrapperArgsSpillsLargeCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cmd := "claude --append-system-prompt '" + strings.Repeat("x", runCommandSpillThreshold+1) + "' task"

	args := runWrapperArgsWith("sid", cmd, "codex", true)
	if len(args) != 7 || args[0] != "run" || args[1] != "--hold" || args[2] != "--adopt" || args[3] != "codex" || args[4] != "--cmd-file" || args[6] != "sid" {
		t.Fatalf("runWrapperArgsWith() = %#v", args)
	}
	data, err := os.ReadFile(args[5])
	if err != nil {
		t.Fatalf("spilled command not readable: %v", err)
	}
	if string(data) != cmd {
		t.Fatalf("spilled command mismatch")
	}
}

func TestRunWrapperShellCommandOmitsLargeInlineCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cmd := "claude --append-system-prompt '" + strings.Repeat("x", runCommandSpillThreshold+1) + "' task"

	got := runWrapperShellCommand("sid", cmd, "", true)
	if !strings.Contains(got, "--cmd-file") {
		t.Fatalf("runWrapperShellCommand did not spill large command: %q", got)
	}
	if strings.Contains(got, strings.Repeat("x", 128)) {
		t.Fatalf("runWrapperShellCommand kept large behavior inline: %q", got)
	}
}

func TestAttachWrapperArgsSpillsLargeCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cmd := "claude --append-system-prompt '" + strings.Repeat("x", runCommandSpillThreshold+1) + "' task"

	args := attachWrapperArgs("sid", cmd, "codex")
	if len(args) != 6 || args[0] != "attach" || args[1] != "sid" || args[2] != "--adopt" || args[3] != "codex" || args[4] != "--cmd-file" {
		t.Fatalf("attachWrapperArgs() = %#v", args)
	}
	data, err := os.ReadFile(args[5])
	if err != nil {
		t.Fatalf("spilled command not readable: %v", err)
	}
	if string(data) != cmd {
		t.Fatalf("spilled command mismatch")
	}
}

func TestAttachWrapperShellCommandOmitsLargeInlineCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cmd := "claude --append-system-prompt '" + strings.Repeat("x", runCommandSpillThreshold+1) + "' task"

	got := attachWrapperShellCommand("sid", cmd, "")
	if !strings.Contains(got, "--cmd-file") {
		t.Fatalf("attachWrapperShellCommand did not spill large command: %q", got)
	}
	if strings.Contains(got, strings.Repeat("x", 128)) {
		t.Fatalf("attachWrapperShellCommand kept large behavior inline: %q", got)
	}
}

// axEnv sets both AX_RUN (current) and AX_GROUP (deprecated alias, same value)
// in the launched child's environment, so anything reading only the old name
// keeps working.
func TestAxEnvSetsRunAndDeprecatedGroupAlias(t *testing.T) {
	env := axEnv("id", "myrun", "parent", 0, 1, fences{}, nil)
	var run, group string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "AX_RUN="); ok {
			run = v
		}
		if v, ok := strings.CutPrefix(e, "AX_GROUP="); ok {
			group = v
		}
	}
	if run != "myrun" || group != "myrun" {
		t.Fatalf("AX_RUN=%q AX_GROUP=%q, want both %q", run, group, "myrun")
	}
}

// launchWindowTitle always folds the group in ahead of --name/harness, so a
// run's windows cluster together in the window list even when --name is set.
func TestLaunchWindowTitle(t *testing.T) {
	cases := []struct {
		name, harness, group, want string
	}{
		{"", "claude", "run1", "run1/claude"},
		{"root", "claude", "run1", "run1/root"},
		{"", "claude", "", "claude"},
		{"root", "claude", "", "root"},
	}
	for _, c := range cases {
		if got := launchWindowTitle(c.name, c.harness, c.group); got != c.want {
			t.Errorf("launchWindowTitle(%q, %q, %q) = %q, want %q", c.name, c.harness, c.group, got, c.want)
		}
	}
}

// A child inherits its parent's freeform labels, strips inherited role, defaults
// role=worker for a spawned child, and lets an explicit --label override keys.
func TestChildInheritsParentLabels(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("AX_LABELS", "")

	// A parent tagged with org labels (including an explicit role its behavior set).
	parentID := "parent-1"
	if err := meta.Save(parentID, meta.Meta{Labels: []string{"project=blog", "area=lan302", "role=lead"}}); err != nil {
		t.Fatal(err)
	}
	inherited := inheritedLabels(parentID)
	if session.LabelValue(inherited, "project") != "blog" {
		t.Fatalf("inheritedLabels lost project: %v", inherited)
	}

	// The child's meta labels: inherited org labels land, and nothing is invented.
	child := childLabels(inherited, nil, parentID)
	if session.LabelValue(child, "project") != "blog" {
		t.Fatalf("child did not inherit project: %v", child)
	}
	if session.LabelValue(child, "area") != "lan302" {
		t.Fatalf("child did not inherit area: %v", child)
	}
	// The role key is never inherited: a role describes what one session IS,
	// not its subtree, so the parent's role must not land on the child; the
	// parented child gets the default worker role instead.
	if got := session.LabelValue(child, "role"); got != "worker" {
		t.Fatalf("child role = %q, want default worker: %v", got, child)
	}

	// An explicit --label overrides an inherited key of the same name, and an
	// explicit role sets the child's own role.
	over := childLabels(inherited, []string{"project=infra", "role=reviewer"}, parentID)
	if got := session.LabelValue(over, "project"); got != "infra" {
		t.Fatalf("explicit label did not override inherited: %q %v", got, over)
	}
	if got := session.LabelValue(over, "role"); got != "reviewer" {
		t.Fatalf("explicit role was not applied: %q %v", got, over)
	}
	if n := strings.Count(strings.Join(over, " "), "project="); n != 1 {
		t.Fatalf("override left a duplicate project key: %v", over)
	}
}

// TestRoleLabelNeverInherited pins the determinism contract for the role key
// through BOTH inheritance sources: the parent's sidecar and the AX_LABELS env
// fallback. A worker spawned by a role=lead session must not inherit that role;
// it defaults to worker unless its own launch passed --label role=...
func TestRoleLabelNeverInherited(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// Sidecar source.
	parent := "role-parent"
	if err := meta.Save(parent, meta.Meta{Labels: []string{"project=blog", "role=lead"}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_LABELS", "")
	child := childLabels(inheritedLabels(parent), nil, parent)
	if got := session.LabelValue(child, "role"); got != "worker" {
		t.Fatalf("sidecar child role = %q, want default worker", got)
	}
	if session.LabelValue(child, "project") != "blog" {
		t.Fatalf("dropping role must not drop other labels: %v", child)
	}

	// Env fallback source (a detached or remote launch that cannot read the
	// parent's sidecar).
	t.Setenv("AX_LABELS", "project=blog\nrole=lead")
	child = childLabels(inheritedLabels("no-such-parent"), nil, "parent-from-env")
	if got := session.LabelValue(child, "role"); got != "worker" {
		t.Fatalf("AX_LABELS child role = %q, want default worker", got)
	}

	// An explicit role on the child's own launch is the only way to get one.
	child = childLabels(inheritedLabels("no-such-parent"), []string{"role=reviewer"}, "parent-from-env")
	if got := session.LabelValue(child, "role"); got != "reviewer" {
		t.Fatalf("explicit role = %q, want reviewer", got)
	}
}

func TestChildLabelsParentlessDoesNotDefaultRole(t *testing.T) {
	child := childLabels([]string{"project=blog", "role=lead"}, nil, "")
	if got := session.LabelValue(child, "role"); got != "" {
		t.Fatalf("parentless childLabels role = %q, want no inherited/default role", got)
	}
}

func TestInheritedGroupParentUsesSessionEnv(t *testing.T) {
	t.Setenv("AX_RUN", "run-env")
	t.Setenv("AX_SESSION_ID", "coord-env")
	group, parent := inheritedGroupParent(launchOpts{})
	if group != "run-env" || parent != "coord-env" {
		t.Fatalf("group=%q parent=%q, want run-env/coord-env", group, parent)
	}
	group, parent = inheritedGroupParent(launchOpts{group: "run-flag", parent: "coord-flag"})
	if group != "run-flag" || parent != "coord-flag" {
		t.Fatalf("explicit group=%q parent=%q, want run-flag/coord-flag", group, parent)
	}
}

// Inheritance falls back to the AX_LABELS env when the parent's sidecar is
// unreadable (a detached or remote launch).
func TestInheritedLabelsEnvFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// No parent sidecar: inheritedLabels reads the AX_LABELS env instead.
	t.Setenv("AX_LABELS", "project=blog\nrole=lead")
	got := inheritedLabels("missing-parent")
	if session.LabelValue(got, "project") != "blog" {
		t.Fatalf("env fallback lost project: %v", got)
	}
}

// Bug fix: the child env must carry this session's OWN final labels, including an
// explicit --label that overrides an inherited key. Before the fix, axEnv was
// handed only the inherited labels, so a nested launch that could not read this
// session's sidecar would inherit the wrong role via the AX_LABELS fallback.
func TestChildEnvCarriesExplicitLabelOverride(t *testing.T) {
	inherited := []string{"project=blog", "role=lead"}
	// The final label set runLaunch computes: inherited folded with an explicit
	// --label role=worker override.
	labels := childLabels(inherited, []string{"role=worker"}, "parent-1")

	env := axEnv("child-1", "grp", "parent-1", 1, 2, fences{}, labels)
	var axLabels string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "AX_LABELS="); ok {
			axLabels = v
		}
	}
	if axLabels == "" {
		t.Fatalf("AX_LABELS not set in child env: %v", env)
	}
	got := splitLabels(axLabels)
	if v := session.LabelValue(got, "role"); v != "worker" {
		t.Fatalf("AX_LABELS did not carry the explicit role override: role=%q in %q", v, axLabels)
	}
	if v := session.LabelValue(got, "project"); v != "blog" {
		t.Fatalf("AX_LABELS lost an inherited label: project=%q in %q", v, axLabels)
	}
}

// initGitRepo creates a bare-minimum git repository at dir (no commits
// needed for rev-parse --show-toplevel or remote get-url to work).
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

// seedProjectLabel prefers the origin remote's repository name over the
// toplevel directory's own name, since it stays stable across worktrees and
// clones that don't share the same directory name.
func TestSeedProjectLabelFromOrigin(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "git@github.com:acme/widgets.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}

	got := seedProjectLabel(nil, dir)
	if v := session.LabelValue(got, "project"); v != "widgets" {
		t.Fatalf("project = %q, want widgets: %v", v, got)
	}
}

// With no origin remote, the toplevel directory's base name is used.
func TestSeedProjectLabelFromToplevel(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	got := seedProjectLabel(nil, dir)
	want := filepath.Base(dir)
	if v := session.LabelValue(got, "project"); v != want {
		t.Fatalf("project = %q, want %q (toplevel base name): %v", v, want, got)
	}
}

// Outside a git repository entirely, the working directory's own base name
// is used.
func TestSeedProjectLabelNotGitRepo(t *testing.T) {
	dir := t.TempDir()

	got := seedProjectLabel(nil, dir)
	want := filepath.Base(dir)
	if v := session.LabelValue(got, "project"); v != want {
		t.Fatalf("project = %q, want %q (dir base name): %v", v, want, got)
	}
}

// An inherited or explicit project label is never overridden by the seeded
// git-derived value, even when the launch directory is a git repo with a
// different name.
func TestSeedProjectLabelDoesNotOverride(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "git@github.com:acme/widgets.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}

	inherited := seedProjectLabel(nil, dir) // sanity: would seed "widgets" if empty
	if session.LabelValue(inherited, "project") != "widgets" {
		t.Fatalf("setup: expected seed to produce widgets, got %v", inherited)
	}

	// A label set that already carries a project (inherited from a parent, or
	// set explicitly) is returned unchanged.
	preset := []string{"project=blog"}
	got := seedProjectLabel(preset, dir)
	if v := session.LabelValue(got, "project"); v != "blog" {
		t.Fatalf("seedProjectLabel overrode an existing project label: got %q, want blog: %v", v, got)
	}
	if len(got) != len(preset) {
		t.Fatalf("seedProjectLabel mutated the label set when project was already set: %v", got)
	}

	// End-to-end through the same fold runLaunch uses: an explicit --label
	// wins over the git-derived seed.
	child := childLabels(nil, []string{"project=infra"}, "parent-1")
	child = seedProjectLabel(child, dir)
	if v := session.LabelValue(child, "project"); v != "infra" {
		t.Fatalf("explicit --label did not win over the seeded project: got %q, want infra: %v", v, child)
	}
}

// muxTargetFor turns a session's labels + the mux_group config into the mux
// session a window should be born in: "off"/"" yield no target (flat), "project"
// reads the seeded project label, an arbitrary key reads that label, and a
// missing key falls back to flat.
func TestMuxTargetFor(t *testing.T) {
	labels := []string{"project=agentswitch", "workstream=picker"}

	// Off (and the default empty) never groups: flat placement in the current
	// mux session, exactly today's behavior.
	if got := muxTargetFor(labels, "off"); got != "" {
		t.Errorf("mux_group=off: target = %q, want \"\" (flat)", got)
	}
	if got := muxTargetFor(labels, ""); got != "" {
		t.Errorf("mux_group unset: target = %q, want \"\" (flat)", got)
	}

	// "project" resolves to the project label value (the mux prefix is applied by
	// the backend at open time, so the target here is the bare value).
	if got := muxTargetFor(labels, "project"); got != "agentswitch" {
		t.Errorf("mux_group=project: target = %q, want agentswitch", got)
	}

	// Any other key groups by that label with no extra code.
	if got := muxTargetFor(labels, "workstream"); got != "picker" {
		t.Errorf("mux_group=workstream: target = %q, want picker", got)
	}

	// A session missing the grouping key collapses to flat.
	if got := muxTargetFor([]string{"role=worker"}, "project"); got != "" {
		t.Errorf("mux_group=project with no project label: target = %q, want \"\" (flat)", got)
	}
	if got := muxTargetFor(nil, "project"); got != "" {
		t.Errorf("mux_group=project with no labels: target = %q, want \"\" (flat)", got)
	}
}

// The default subscription path strips the Anthropic API-key env from the child so
// a launch can never silently burn pay-as-you-go credits; --api opts back in, and
// a non-Anthropic harness is left untouched.
func TestApplyEnvPolicyAuth(t *testing.T) {
	cmd := "claude --session-id x --model sonnet task"

	// The rendered statements are the platform shell's forms: POSIX sh on unix,
	// PowerShell on Windows (see internal/shell).
	unsetBoth := "unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN; "
	setFromNamed := `export ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY_WORK"`
	unsetToken := "unset ANTHROPIC_AUTH_TOKEN"
	if runtime.GOOS == "windows" {
		unsetBoth = "Remove-Item Env:ANTHROPIC_API_KEY, Env:ANTHROPIC_AUTH_TOKEN -ErrorAction SilentlyContinue; "
		setFromNamed = "$env:ANTHROPIC_API_KEY = $env:ANTHROPIC_API_KEY_WORK"
		unsetToken = "Remove-Item Env:ANTHROPIC_AUTH_TOKEN"
	}

	// Default: subscription strips the key env (the old stripAPIAuth behavior).
	got := applyEnvPolicy(cmd, launchOpts{}, "claude", nil)
	if !strings.HasPrefix(got, unsetBoth) {
		t.Fatalf("subscription did not unset the key env: %q", got)
	}
	if !strings.HasSuffix(got, cmd) {
		t.Fatalf("subscription dropped the harness command: %q", got)
	}

	// --api passes the ambient key through: no unset, command untouched.
	if got := applyEnvPolicy(cmd, launchOpts{api: true}, "claude", nil); got != cmd {
		t.Fatalf("--api should pass the env through untouched, got %q", got)
	}

	// --auth api is the same as --api.
	if got := applyEnvPolicy(cmd, launchOpts{auth: "api"}, "claude", nil); got != cmd {
		t.Fatalf("--auth api should pass the env through, got %q", got)
	}

	// --auth env:VAR forces the key from a named variable and drops AUTH_TOKEN.
	got = applyEnvPolicy(cmd, launchOpts{auth: "env:ANTHROPIC_API_KEY_WORK"}, "claude", nil)
	if !strings.Contains(got, setFromNamed) {
		t.Fatalf("env: auth did not set the key from the named var: %q", got)
	}
	if !strings.Contains(got, unsetToken) {
		t.Fatalf("env: auth did not drop AUTH_TOKEN: %q", got)
	}

	// A non-Anthropic harness (codex) is left untouched even under the default.
	if got := applyEnvPolicy(cmd, launchOpts{}, "codex", nil); got != cmd {
		t.Fatalf("codex auth env must be untouched, got %q", got)
	}
}

// --env overrides are exported for the child on top of the inherited environment,
// with values shell-quoted so spaces survive.
func TestApplyEnvPolicyOverrides(t *testing.T) {
	cmd := "codex task"
	setFoo, setBaz := "export FOO='bar'", "export BAZ='a b'"
	if runtime.GOOS == "windows" {
		setFoo, setBaz = "$env:FOO = 'bar'", "$env:BAZ = 'a b'"
	}
	got := applyEnvPolicy(cmd, launchOpts{envSet: []string{"FOO=bar", "BAZ=a b"}}, "codex", nil)
	if !strings.Contains(got, setFoo) {
		t.Fatalf("override FOO not exported: %q", got)
	}
	if !strings.Contains(got, setBaz) {
		t.Fatalf("override BAZ not shell-quoted: %q", got)
	}
	if !strings.HasSuffix(got, cmd) {
		t.Fatalf("overrides dropped the command: %q", got)
	}
}

// --clean-env wraps the command in `env -i` with the keep allowlist, re-injects the
// AX_* control vars, and (for the default subscription) never re-adds the key env.
func TestApplyEnvPolicyCleanEnv(t *testing.T) {
	cmd := "claude task"
	ax := []string{"AX_SESSION_ID=abc", "AX_RUN=run"}
	got := applyEnvPolicy(cmd, launchOpts{cleanEnv: true}, "claude", ax)
	if runtime.GOOS == "windows" {
		// PowerShell has no env -i: the renderer snapshots the keep allowlist,
		// wipes Env:, reapplies, and runs the command in the same shell
		// (see shell.CleanEnv on windows).
		if !strings.HasPrefix(got, "$__axenv0 = $env:") {
			t.Fatalf("clean-env did not snapshot the keep allowlist first: %q", got)
		}
		if !strings.Contains(got, `Get-ChildItem Env: | ForEach-Object { Remove-Item "Env:$($_.Name)" }`) {
			t.Fatalf("clean-env did not wipe the inherited environment: %q", got)
		}
		if !strings.Contains(got, "$env:PATH = $__axenv") {
			t.Fatalf("clean-env dropped PATH from the keep allowlist: %q", got)
		}
		if !strings.Contains(got, "$env:AX_SESSION_ID = 'abc'") || !strings.Contains(got, "$env:AX_RUN = 'run'") {
			t.Fatalf("clean-env did not re-inject the AX_* control vars: %q", got)
		}
		if strings.Contains(got, "ANTHROPIC_API_KEY") {
			t.Fatalf("clean-env subscription must not re-add the key env: %q", got)
		}
		if !strings.HasSuffix(got, "; "+cmd) {
			t.Fatalf("clean-env did not end with the command: %q", got)
		}
	} else {
		if !strings.HasPrefix(got, "env -i ") {
			t.Fatalf("clean-env did not start from env -i: %q", got)
		}
		if !strings.Contains(got, `PATH="$PATH"`) {
			t.Fatalf("clean-env dropped PATH from the keep allowlist: %q", got)
		}
		if !strings.Contains(got, "AX_SESSION_ID='abc'") || !strings.Contains(got, "AX_RUN='run'") {
			t.Fatalf("clean-env did not re-inject the AX_* control vars: %q", got)
		}
		if strings.Contains(got, "ANTHROPIC_API_KEY") {
			t.Fatalf("clean-env subscription must not re-add the key env: %q", got)
		}
		if !strings.HasSuffix(got, "sh -c "+shellQuote(cmd)) {
			t.Fatalf("clean-env did not run the command under sh -c: %q", got)
		}
	}

	// Under clean-env, api re-adds the key from the ambient env; env:VAR from a name.
	// On Windows the value rides a snapshot variable taken before the wipe.
	wantAmbient, wantNamedSrc, wantNamedSet := `ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY"`, `ANTHROPIC_API_KEY="$MY_KEY"`, ""
	if runtime.GOOS == "windows" {
		wantAmbient = "$env:ANTHROPIC_API_KEY = $__axenv"
		wantNamedSrc = " = $env:MY_KEY"
		wantNamedSet = "$env:ANTHROPIC_API_KEY = $__axenv"
	}
	got = applyEnvPolicy(cmd, launchOpts{cleanEnv: true, auth: "api"}, "claude", ax)
	if !strings.Contains(got, wantAmbient) {
		t.Fatalf("clean-env api did not re-add the ambient key: %q", got)
	}
	got = applyEnvPolicy(cmd, launchOpts{cleanEnv: true, auth: "env:MY_KEY"}, "claude", ax)
	if !strings.Contains(got, wantNamedSrc) {
		t.Fatalf("clean-env env: did not re-add the named key: %q", got)
	}
	if wantNamedSet != "" && !strings.Contains(got, wantNamedSet) {
		t.Fatalf("clean-env env: did not assign the key from its snapshot: %q", got)
	}
}

// The autonomous (non-fenced) launch injects each harness's own permission-bypass
// flag from its config, not a hardcoded claude-specific one, so a watched worker
// does not hang on tool prompts but a harness with no such prompt (or its own
// headless bypass) gets nothing.
func TestAutonomyBypass(t *testing.T) {
	for _, h := range config.Default().Harnesses {
		got := autonomyBypass(h)
		if got != h.SkipPermissions {
			t.Fatalf("%s: autonomyBypass = %q, want its own SkipPermissions %q", h.Name, got, h.SkipPermissions)
		}
	}

	claude := autonomyBypass(config.Harness{Format: "claude", SkipPermissions: "--dangerously-skip-permissions"})
	if claude != "--dangerously-skip-permissions" {
		t.Fatalf("claude bypass = %q", claude)
	}

	pi := autonomyBypass(config.Harness{Format: "pi", SkipPermissions: ""})
	if pi != "" {
		t.Fatalf("pi bypass = %q, want empty (pi has no per-tool permission prompt)", pi)
	}
	if pi == claude {
		t.Fatal("pi must not receive claude's --dangerously-skip-permissions flag")
	}

	if got := autonomyBypass(config.Harness{Format: "codex", SkipPermissions: ""}); got != "" {
		t.Fatalf("codex should carry its own bypass in its headless template, got %q", got)
	}
}

// The launcher flags for the two axes parse into the expected opts: --headless
// opts into the screenless job form; --wait/--unattended are independent (block /
// no-human) and do NOT; --api opts in to key billing.
func TestParseLaunchModeAndBilling(t *testing.T) {
	o, err := parseLaunch([]string{"do a thing", "--headless", "--api", "--keep-live"})
	if err != nil {
		t.Fatal(err)
	}
	if o.task != "do a thing" {
		t.Fatalf("task = %q", o.task)
	}
	if !o.headless || !o.api || !o.keepLive {
		t.Fatalf("expected headless+api+keep-live, got headless=%v api=%v keepLive=%v", o.headless, o.api, o.keepLive)
	}
	// Default: neither flag set means interactive + subscription.
	d, err := parseLaunch([]string{"task"})
	if err != nil {
		t.Fatal(err)
	}
	if d.headless || d.api || d.wait || d.unattend {
		t.Fatalf("default launch should be interactive+subscription: %+v", d)
	}
}

// launchMode is the attachability contract: a --wait/--unattended job runs the
// INTERACTIVE command (its live TUI, attachable under the holder), NOT the
// screenless `claude -p` headless form; only an explicit --headless selects -p.
// This is the regression guard for the blank-screen-on-attach bug: --wait used to
// imply headless, so dropping into a --wait job showed nothing.
func TestLaunchModeAttachableForWaitAndUnattended(t *testing.T) {
	h := config.Harness{
		Name:           "claude",
		Launch:         "claude --session-id {newid} --model {model} {task}",
		LaunchHeadless: "claude -p --session-id {newid} --model {model} {task}",
	}
	const task = "do the thing"

	cases := []struct {
		name string
		o    launchOpts
		mode string
	}{
		{"default", launchOpts{}, "interactive"},
		{"wait", launchOpts{wait: true}, "interactive"},
		{"unattended", launchOpts{unattend: true}, "interactive"},
		{"wait+unattended", launchOpts{wait: true, unattend: true}, "interactive"},
		{"headless", launchOpts{headless: true}, "headless"},
		{"headless+wait", launchOpts{headless: true, wait: true}, "headless"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmpl, mode := launchMode(h, c.o, task)
			if mode != c.mode {
				t.Fatalf("mode = %q, want %q", mode, c.mode)
			}
			switch c.mode {
			case "interactive":
				if tmpl != h.Launch {
					t.Fatalf("interactive should use the Launch template, got %q", tmpl)
				}
				if strings.Contains(tmpl, " -p ") || strings.HasSuffix(tmpl, " -p") {
					t.Fatalf("interactive command must not be the screenless -p form: %q", tmpl)
				}
			case "headless":
				if tmpl != h.LaunchHeadless {
					t.Fatalf("headless should use the LaunchHeadless template, got %q", tmpl)
				}
				if !strings.Contains(tmpl, " -p ") {
					t.Fatalf("headless command must be the screenless -p form: %q", tmpl)
				}
			}
		})
	}

	// A taskless launch (a human at the wheel) always stays interactive even with
	// --headless, since there is no task to run to completion.
	if _, mode := launchMode(h, launchOpts{headless: true}, ""); mode != "interactive" {
		t.Fatalf("taskless --headless should stay interactive, got %q", mode)
	}
	// A harness with no headless template can never go headless.
	noHeadless := config.Harness{Name: "pi", Launch: "pi {task}"}
	if _, mode := launchMode(noHeadless, launchOpts{headless: true}, task); mode != "interactive" {
		t.Fatalf("harness without a headless template should stay interactive, got %q", mode)
	}
}

// The env and auth flags parse and validate: --clean-env, repeatable --env
// KEY=VALUE (rejecting a malformed pair), and --auth normalizing sub/api/env:VAR
// and rejecting garbage.
func TestParseLaunchEnvAndAuth(t *testing.T) {
	o, err := parseLaunch([]string{"task", "--clean-env", "--env", "FOO=bar", "--env", "BAZ=qux", "--auth", "sub"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.cleanEnv {
		t.Fatal("--clean-env did not set cleanEnv")
	}
	if len(o.envSet) != 2 || o.envSet[0] != "FOO=bar" || o.envSet[1] != "BAZ=qux" {
		t.Fatalf("--env not accumulated: %v", o.envSet)
	}
	if o.auth != "subscription" {
		t.Fatalf("--auth sub did not normalize to subscription: %q", o.auth)
	}

	if _, err := parseLaunch([]string{"t", "--env", "NOTAPAIR"}); err == nil {
		t.Fatal("--env without = should error")
	}
	if _, err := parseLaunch([]string{"t", "--env", "1BAD=x"}); err == nil {
		t.Fatal("--env with an invalid name should error")
	}
	if _, err := parseLaunch([]string{"t", "--auth", "nonsense"}); err == nil {
		t.Fatal("--auth nonsense should error")
	}
	o, err = parseLaunch([]string{"t", "--auth", "env:ANTHROPIC_API_KEY_WORK"})
	if err != nil {
		t.Fatal(err)
	}
	if o.auth != "env:ANTHROPIC_API_KEY_WORK" {
		t.Fatalf("--auth env: not preserved: %q", o.auth)
	}
	if _, err := parseLaunch([]string{"t", "--auth", "env:bad name"}); err == nil {
		t.Fatal("--auth env: with an invalid name should error")
	}
}

// resolveAuth reduces the flags to one source: explicit --auth wins, then legacy
// --api, then the subscription default.
func TestResolveAuth(t *testing.T) {
	if got := resolveAuth(launchOpts{}); got != "subscription" {
		t.Fatalf("default = %q, want subscription", got)
	}
	if got := resolveAuth(launchOpts{api: true}); got != "api" {
		t.Fatalf("--api = %q, want api", got)
	}
	if got := resolveAuth(launchOpts{api: true, auth: "subscription"}); got != "subscription" {
		t.Fatalf("explicit --auth should win over --api, got %q", got)
	}
}

// A launch spec round-trips through specFromOpts/optsFromSpec: the fields ax needs
// to reconstruct a session survive intact.
func TestSpecRoundTrip(t *testing.T) {
	o := launchOpts{
		task: "do it", behavior: "reviewer", model: "sonnet", name: "w1",
		dir: "/tmp/x", accept: "./check.sh", labels: []string{"project=blog"},
		hflags: []string{"--foo"}, headless: true, unattend: true, closeOnDone: true, keepLive: true,
		fenceMode: "best-effort", cleanEnv: true,
		envSet: []string{"K=V"}, auth: "env:MY_KEY",
	}
	o.fen.writeGlobs = []string{"./out/**/*.md"}
	o.fen.maxCost = 5
	o.fen.maxWorkers = 3
	o.fen.timeout = 90 * time.Second

	sp := specFromOpts("claude", o, "grp", "par", "human")
	if sp.Harness != "claude" || sp.Group != "grp" || sp.Parent != "par" || sp.Origin != "human" {
		t.Fatalf("identity lost: %+v", sp)
	}
	if sp.Timeout != "1m30s" {
		t.Fatalf("timeout not serialized: %q", sp.Timeout)
	}

	got := optsFromSpec(sp)
	if got.task != o.task || got.behavior != o.behavior || got.model != o.model {
		t.Fatalf("core fields lost: %+v", got)
	}
	if !got.cleanEnv || got.auth != "env:MY_KEY" || len(got.envSet) != 1 {
		t.Fatalf("env/auth policy lost: %+v", got)
	}
	if len(got.fen.writeGlobs) != 1 || got.fen.writeGlobs[0] != "./out/**/*.md" || got.fen.maxCost != 5 || got.fen.maxWorkers != 3 || got.fen.timeout != 90*time.Second {
		t.Fatalf("fences lost: %+v", got.fen)
	}
	if !got.headless || !got.unattend || !got.closeOnDone || !got.keepLive || got.fenceMode != "best-effort" {
		t.Fatalf("mode/policy flags lost: %+v", got)
	}
}

// Effort survives a specFromOpts/optsFromSpec round-trip so `ax restart` preserves it.
func TestSpecRoundTripEffort(t *testing.T) {
	o := launchOpts{task: "task", effort: "xhigh"}
	sp := specFromOpts("claude", o, "g", "", "human")
	if sp.Effort != "xhigh" {
		t.Fatalf("specFromOpts: Effort = %q, want %q", sp.Effort, "xhigh")
	}
	got := optsFromSpec(sp)
	if got.effort != "xhigh" {
		t.Fatalf("optsFromSpec: effort = %q, want %q", got.effort, "xhigh")
	}

	// Empty effort also round-trips cleanly.
	o2 := launchOpts{task: "task"}
	sp2 := specFromOpts("claude", o2, "g", "", "human")
	if sp2.Effort != "" {
		t.Fatalf("specFromOpts: empty Effort = %q, want empty", sp2.Effort)
	}
	got2 := optsFromSpec(sp2)
	if got2.effort != "" {
		t.Fatalf("optsFromSpec: empty effort = %q, want empty", got2.effort)
	}
}

// Self-propel flags survive a specFromOpts/optsFromSpec round-trip, so the run
// wrapper reads the pump config off the persisted spec and `ax restart` keeps a
// self-propelled coordinator self-propelled.
func TestSpecRoundTripSelfPropel(t *testing.T) {
	o := launchOpts{
		task: "coordinate", selfPropel: true, propelPrompt: "keep going",
		propelDone: "./done.sh", propelMaxIdle: 5, propelBackoff: 2 * time.Second,
		propelWatch: "notes/tasks.md",
	}
	sp := specFromOpts("pi", o, "g", "", "human")
	if !sp.SelfPropel || sp.PropelPrompt != "keep going" || sp.PropelDone != "./done.sh" ||
		sp.PropelMaxIdle != 5 || sp.PropelBackoff != "2s" || sp.PropelWatch != "notes/tasks.md" {
		t.Fatalf("self-propel not serialized: %+v", sp)
	}
	got := optsFromSpec(sp)
	if !got.selfPropel || got.propelPrompt != "keep going" || got.propelDone != "./done.sh" ||
		got.propelMaxIdle != 5 || got.propelBackoff != 2*time.Second || got.propelWatch != "notes/tasks.md" {
		t.Fatalf("self-propel lost on restore: %+v", got)
	}

	// The flag-absent path: a launch with no --self-propel serializes nothing, so
	// a plain session is never accidentally propelled after a restart.
	plain := specFromOpts("pi", launchOpts{task: "x"}, "g", "", "human")
	if plain.SelfPropel || plain.PropelPrompt != "" || plain.PropelDone != "" ||
		plain.PropelMaxIdle != 0 || plain.PropelBackoff != "" || plain.PropelWatch != "" {
		t.Fatalf("flag-absent launch carried propel state: %+v", plain)
	}
}

// parseLaunch reads the self-propel flags (and the --done-check/--propel-max-idle
// aliases); their absence leaves the launch entirely unpropelled.
func TestParseLaunchSelfPropel(t *testing.T) {
	o, err := parseLaunch([]string{
		"task", "--self-propel", "--propel-prompt", "go on",
		"--done-check", "./chk.sh", "--max-idle-turns", "4", "--propel-backoff", "10s",
		"--propel-watch", "notes/tasks.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !o.selfPropel || o.propelPrompt != "go on" || o.propelDone != "./chk.sh" ||
		o.propelMaxIdle != 4 || o.propelBackoff != 10*time.Second || o.propelWatch != "notes/tasks.md" {
		t.Fatalf("self-propel flags not parsed: %+v", o)
	}

	plain, err := parseLaunch([]string{"task"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.selfPropel || plain.propelPrompt != "" || plain.propelDone != "" ||
		plain.propelMaxIdle != 0 || plain.propelBackoff != 0 || plain.propelWatch != "" {
		t.Fatalf("flag-absent launch is not plain: %+v", plain)
	}

	if _, err := parseLaunch([]string{"t", "--max-idle-turns", "notanumber"}); err == nil {
		t.Fatal("--max-idle-turns with a non-number must error")
	}
}

func TestSpecRoundTripBehaviorText(t *testing.T) {
	o := launchOpts{task: "task", behaviorText: "inline\nbehavior"}
	sp := specFromOpts("claude", o, "g", "", "human")
	if sp.BehaviorText != "inline\nbehavior" {
		t.Fatalf("specFromOpts: BehaviorText = %q", sp.BehaviorText)
	}
	got := optsFromSpec(sp)
	if got.behaviorText != o.behaviorText {
		t.Fatalf("optsFromSpec: behaviorText = %q, want %q", got.behaviorText, o.behaviorText)
	}
}

// pretrustDir writes projects[dir].hasTrustDialogAccepted into ~/.claude.json for a
// fresh dir, is a no-op for an already-trusted dir, and only touches the claude
// harness. HOME is redirected so the test never touches the real config.
func TestPretrustDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads USERPROFILE on Windows
	cfgPath := filepath.Join(home, ".claude.json")
	work := t.TempDir()

	// Seed a config with an unrelated project so we confirm it is preserved.
	seed := `{"numStartups":3,"projects":{"/other":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(cfgPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	// Non-claude harness: no write.
	pretrustDir("codex", work)
	// claude: pre-trusts the fresh dir.
	pretrustDir("claude", work)

	var top map[string]any
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("config no longer valid json: %v", err)
	}
	if _, ok := top["numStartups"]; !ok {
		t.Fatal("pretrustDir clobbered unrelated top-level keys")
	}
	projects, _ := top["projects"].(map[string]any)
	if projects == nil {
		t.Fatal("projects missing")
	}
	if other, _ := projects["/other"].(map[string]any); other == nil || other["hasTrustDialogAccepted"] != true {
		t.Fatal("pretrustDir dropped the unrelated project entry")
	}
	// The trust key must be the SYMLINK-RESOLVED path: claude keys its trust
	// map by the real path (on macOS /tmp and /var are symlinks into /private),
	// so an unresolved key misses and the dialog still blocks the worker.
	abs, _ := filepath.Abs(work)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	proj, _ := projects[abs].(map[string]any)
	if proj == nil || proj["hasTrustDialogAccepted"] != true {
		t.Fatalf("fresh dir was not pre-trusted under its resolved path: %v", projects)
	}
}

// pretrustDir for codex appends [projects."<resolved>"] trust_level = "trusted"
// to ~/.codex/config.toml (the block codex writes when you answer its trust
// dialog), preserves existing content, is idempotent, and never touches claude's
// config. Without this a driven codex session hangs invisibly on the dialog.
func TestPretrustCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed an existing config so we confirm it is preserved, not rewritten.
	seed := "model = \"gpt-5.5\"\n\n[projects.\"/other\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(cfgPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	abs, _ := filepath.Abs(work)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}

	pretrustDir("codex", work)
	pretrustDir("codex", work) // idempotent: a second call must not duplicate the block

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "model = \"gpt-5.5\"") || !strings.Contains(got, "[projects.\"/other\"]") {
		t.Fatalf("pretrustCodex clobbered existing config:\n%s", got)
	}
	// pretrustCodex TOML-escapes backslash and quote in the path key (a no-op on
	// POSIX paths, but Windows paths carry backslashes), so match that form.
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(abs)
	header := "[projects.\"" + esc + "\"]"
	if n := strings.Count(got, header); n != 1 {
		t.Fatalf("expected exactly one trust block for the work dir, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, header+"\ntrust_level = \"trusted\"") {
		t.Fatalf("trust block malformed:\n%s", got)
	}
}
