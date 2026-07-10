package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/finder"
	"github.com/agentswitch-org/ax/internal/view"
)

// queuedFinder scripts the sequential compose modals: Choose pops the next
// queued return item (recording each call's header/items), PromptMultiline pops
// the next queued (text, ok), and ChooseDir returns a fixed directory. It embeds
// finder.Finder so unused methods panic loudly if the flow calls them.
type mlResp struct {
	text string
	ok   bool
}

type queuedFinder struct {
	finder.Finder
	chooseQ       []string
	chooseHeaders []string
	chooseItems   [][]string
	dir           string
	dirOK         bool
	mlQ           []mlResp
	mlLabels      []string
}

func (f *queuedFinder) Choose(_, header string, items, _ []string) (string, string, error) {
	f.chooseHeaders = append(f.chooseHeaders, header)
	f.chooseItems = append(f.chooseItems, append([]string(nil), items...))
	if len(f.chooseQ) == 0 {
		return "", "", nil
	}
	r := f.chooseQ[0]
	f.chooseQ = f.chooseQ[1:]
	return r, "", nil
}

func (f *queuedFinder) ChooseDir(_, _ string, _ func(string) []string, _ []string) (string, string, error) {
	if !f.dirOK {
		return "", "", nil
	}
	return f.dir, "", nil
}

func (f *queuedFinder) PromptMultiline(label, _, _ string) (string, bool) {
	f.mlLabels = append(f.mlLabels, label)
	if len(f.mlQ) == 0 {
		return "", false
	}
	r := f.mlQ[0]
	f.mlQ = f.mlQ[1:]
	return r.text, r.ok
}

// captureComposeLaunch swaps the MODE 2 launch seam for a recorder, so a behavior
// compose test can assert the composed launchOpts without a real harness launch.
func captureComposeLaunch(t *testing.T) *struct {
	called  bool
	harness string
	opts    launchOpts
} {
	t.Helper()
	rec := &struct {
		called  bool
		harness string
		opts    launchOpts
	}{}
	orig := composeLaunchFn
	composeLaunchFn = func(_ App, harness string, o launchOpts) {
		rec.called, rec.harness, rec.opts = true, harness, o
	}
	t.Cleanup(func() { composeLaunchFn = orig })
	return rec
}

func claudeCfg(behaviorsDir, recipesDir string) config.Config {
	return config.Config{
		Harnesses:    []config.Harness{claudeHarness()},
		BehaviorsDir: behaviorsDir,
		RecipesDir:   recipesDir,
	}
}

// MODE 2 with a behavior FILE launches --behavior <abs path>, the multiline task
// as the prompt, and the chosen dir; no inline text.
func TestComposeBehaviorFileLaunch(t *testing.T) {
	behDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(behDir, "coord.md"), []byte("be a good lead"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rec := captureComposeLaunch(t)

	f := &queuedFinder{
		chooseQ: []string{"claude", composeModeBehavior, "coord.md"},
		dir:     dir, dirOK: true,
		mlQ: []mlResp{{text: "do the migration", ok: true}},
	}
	a := App{mux: inactiveMux{}, find: f, dirs: staticDirs{dir}}
	a.compose(claudeCfg(behDir, ""), nil, nil)

	if !rec.called {
		t.Fatal("behavior compose must launch")
	}
	if rec.harness != "claude" {
		t.Fatalf("harness = %q, want claude", rec.harness)
	}
	if want := filepath.Join(behDir, "coord.md"); rec.opts.behavior != want {
		t.Fatalf("behavior = %q, want %q", rec.opts.behavior, want)
	}
	if rec.opts.behaviorText != "" {
		t.Fatalf("behaviorText = %q, want empty for a file behavior", rec.opts.behaviorText)
	}
	if rec.opts.task != "do the migration" {
		t.Fatalf("task = %q, want the multiline prompt", rec.opts.task)
	}
	if rec.opts.dir != dir {
		t.Fatalf("dir = %q, want %q", rec.opts.dir, dir)
	}
}

// MODE 2 with INLINE behavior launches --behavior-text (no --behavior path) plus
// the task prompt.
func TestComposeBehaviorInlineLaunch(t *testing.T) {
	dir := t.TempDir()
	rec := captureComposeLaunch(t)

	f := &queuedFinder{
		chooseQ: []string{"claude", composeModeBehavior, behaviorInline},
		dir:     dir, dirOK: true,
		mlQ: []mlResp{{text: "always write tests", ok: true}, {text: "ship it", ok: true}},
	}
	a := App{mux: inactiveMux{}, find: f, dirs: staticDirs{dir}}
	a.compose(claudeCfg(t.TempDir(), ""), nil, nil)

	if !rec.called {
		t.Fatal("inline behavior compose must launch")
	}
	if rec.opts.behavior != "" {
		t.Fatalf("behavior path = %q, want empty for inline", rec.opts.behavior)
	}
	if rec.opts.behaviorText != "always write tests" {
		t.Fatalf("behaviorText = %q, want the inline text", rec.opts.behaviorText)
	}
	if rec.opts.task != "ship it" {
		t.Fatalf("task = %q, want the prompt", rec.opts.task)
	}
}

// MODE 2 with SKIP launches with no behavior flag at all.
func TestComposeBehaviorSkipLaunch(t *testing.T) {
	dir := t.TempDir()
	rec := captureComposeLaunch(t)

	f := &queuedFinder{
		chooseQ: []string{"claude", composeModeBehavior, behaviorSkip},
		dir:     dir, dirOK: true,
		mlQ: []mlResp{{text: "just the task", ok: true}},
	}
	a := App{mux: inactiveMux{}, find: f, dirs: staticDirs{dir}}
	a.compose(claudeCfg("", ""), nil, nil)

	if !rec.called {
		t.Fatal("skip behavior compose must launch")
	}
	if rec.opts.behavior != "" || rec.opts.behaviorText != "" {
		t.Fatalf("skip must set no behavior: behavior=%q text=%q", rec.opts.behavior, rec.opts.behaviorText)
	}
	if rec.opts.task != "just the task" {
		t.Fatalf("task = %q", rec.opts.task)
	}
}

// With behaviors_dir unset the behavior step still offers inline + skip (and no
// files), and inline still launches --behavior-text.
func TestComposeBehaviorUnsetDirStillOffersInlineAndSkip(t *testing.T) {
	dir := t.TempDir()
	rec := captureComposeLaunch(t)

	f := &queuedFinder{
		chooseQ: []string{"claude", composeModeBehavior, behaviorInline},
		dir:     dir, dirOK: true,
		mlQ: []mlResp{{text: "inline only", ok: true}, {text: "task", ok: true}},
	}
	a := App{mux: inactiveMux{}, find: f, dirs: staticDirs{dir}}
	a.compose(claudeCfg("", ""), nil, nil)

	// The behavior chooser is the 3rd Choose call (target skipped, harness, mode,
	// behavior). Its items are exactly inline + skip when behaviors_dir is unset.
	behaviorItems := f.chooseItems[len(f.chooseItems)-1]
	if len(behaviorItems) != 2 || behaviorItems[0] != behaviorInline || behaviorItems[1] != behaviorSkip {
		t.Fatalf("behavior items with unset dir = %v, want [inline, skip]", behaviorItems)
	}
	if !rec.called || rec.opts.behaviorText != "inline only" {
		t.Fatalf("inline launch text = %q (called=%v)", rec.opts.behaviorText, rec.called)
	}
}

// Cancelling the task prompt (Esc in the multiline editor) aborts the launch.
func TestComposeBehaviorPromptCancelAborts(t *testing.T) {
	dir := t.TempDir()
	rec := captureComposeLaunch(t)

	f := &queuedFinder{
		chooseQ: []string{"claude", composeModeBehavior, behaviorSkip},
		dir:     dir, dirOK: true,
		mlQ: []mlResp{{text: "", ok: false}}, // task prompt cancelled
	}
	a := App{mux: inactiveMux{}, find: f, dirs: staticDirs{dir}}
	a.compose(claudeCfg("", ""), nil, nil)

	if rec.called {
		t.Fatal("a cancelled task prompt must not launch")
	}
}

// MODE 3 picks a recipe and runs it tracked via RunRecipe: the recipe path, the
// picked harness (as AX_HARNESS), and the chosen dir reach the launch.
func TestComposeRecipeRunsPickedRecipe(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeAppCfg(t, "")
	recDir := t.TempDir()
	recipePath := filepath.Join(recDir, "smoke.sh")
	if err := os.WriteFile(recipePath, []byte("# ax: name = Smoke\n# ax: description = prints ok\necho ok\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	mx := &recordingActiveMux{}
	f := &queuedFinder{
		chooseQ: []string{"claude", composeModeRecipe, "Smoke · prints ok"},
		dir:     dir, dirOK: true,
	}
	a := App{mux: mx, find: f, dirs: staticDirs{dir}}
	a.compose(claudeCfg("", recDir), nil, nil)

	if len(mx.opens) != 1 {
		t.Fatalf("recipe compose must open exactly one tracked run, got %d", len(mx.opens))
	}
	call := mx.opens[0]
	if call.dir != dir {
		t.Fatalf("recipe run dir = %q, want %q", call.dir, dir)
	}
	if !strings.Contains(call.cmd, "AX_HARNESS") || !strings.Contains(call.cmd, "claude") {
		t.Fatalf("recipe command must export AX_HARNESS=claude: %q", call.cmd)
	}
	if !strings.Contains(call.cmd, recipePath) {
		t.Fatalf("recipe command must reference the picked recipe %q: %q", recipePath, call.cmd)
	}
	if !strings.Contains(call.title, "/Smoke") {
		t.Fatalf("recipe window title = %q, want the recipe name", call.title)
	}
}

// MODE 3 with recipes_dir unset shows a clear message and returns cleanly,
// launching nothing.
func TestComposeRecipeUnsetDirIsCleanNoOp(t *testing.T) {
	mx := &recordingActiveMux{}
	f := &queuedFinder{
		chooseQ: []string{"claude", composeModeRecipe},
	}
	a := App{mux: mx, find: f, dirs: staticDirs{}}
	a.compose(claudeCfg("", ""), nil, nil)

	if len(mx.opens) != 0 {
		t.Fatalf("unset recipes_dir must launch nothing, got %d opens", len(mx.opens))
	}
	// The last Choose is the notify modal, whose header names the missing config.
	last := f.chooseHeaders[len(f.chooseHeaders)-1]
	if !strings.Contains(last, "recipes_dir") {
		t.Fatalf("notify header = %q, want a recipes_dir message", last)
	}
}

// MODE 1 (plain) routes to the existing New path: harness picked in compose, dir
// picked, harness launched (here via the no-window in-terminal exec path).
func TestComposePlainRoutesToNew(t *testing.T) {
	writeAppCfg(t, "[[harness]]\nname = \"claude\"\n")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	dir := t.TempDir()
	held, _, _ := captureNewExec(t)

	f := &queuedFinder{
		chooseQ: []string{"claude", composeModePlain},
		dir:     dir, dirOK: true,
	}
	a := App{mux: &recordingActiveMux{}, find: f, dirs: staticDirs{dir}}
	a.compose(claudeCfg("", ""), nil, nil)

	if len(*held) != 1 {
		t.Fatalf("plain compose must launch a plain new session once, got %d", len(*held))
	}
	if _, cmd, _ := strings.Cut((*held)[0], "\x00"); !strings.Contains(cmd, "claude --session-id") {
		t.Fatalf("plain launch cmd = %q, want a claude new-session launch", cmd)
	}
}

// A remote compose target aborts with an error instead of degrading to a plain
// remote `ax new`, which would discard the selected compose mode and inputs.
func TestComposeRemoteTargetAbortsInsteadOfPlainRemoteNew(t *testing.T) {
	rec := captureComposeLaunch(t)
	f := &queuedFinder{chooseQ: []string{"remote"}}
	a := App{mux: inactiveMux{}, find: f}
	hosts := []view.HostStatus{
		{Name: "local", State: view.HostLocal},
		{Name: "remote", State: view.HostOnline},
	}
	hostByName := map[string]config.Host{
		"remote": {Name: "remote", Transport: "ssh remote"},
	}

	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			a.compose(claudeCfg("", ""), hosts, hostByName)
		})
	})

	if rec.called {
		t.Fatal("remote compose must not launch a composed local session")
	}
	if stdout != "" {
		t.Fatalf("remote compose must not print a remote ax new command, got %q", stdout)
	}
	if !strings.Contains(stderr, "remote compose is not yet supported") || !strings.Contains(stderr, "remote") {
		t.Fatalf("stderr = %q, want clear remote compose unsupported error", stderr)
	}
	if len(f.chooseItems) != 1 {
		t.Fatalf("compose should abort after target selection, got %d Choose calls", len(f.chooseItems))
	}
}

// act() dispatches a picker Compose choice (c/C) into the compose entrypoint,
// distinct from a plain New/NewArgs choice. This is the wiring the per-shape
// compose tests skip by calling a.compose directly: it proves the c/C choice
// reaches compose through act's routing. A harness pick returning "" aborts,
// so exactly one Choose (the harness list) fires and nothing launches.
func TestActRoutesComposeChoiceIntoComposeFlow(t *testing.T) {
	rec := captureComposeLaunch(t)
	f := &queuedFinder{chooseQ: nil} // harness pick returns "" -> compose aborts after entering
	a := App{mux: inactiveMux{}, find: f}
	a.act(finder.Choice{Compose: true, Config: claudeCfg("", "")})

	if rec.called {
		t.Fatal("an aborted compose must not launch")
	}
	if len(f.chooseItems) != 1 {
		t.Fatalf("act(Compose) should enter compose and stop at the harness pick, got %d Choose calls", len(f.chooseItems))
	}
	if items := f.chooseItems[0]; len(items) != 1 || items[0] != "claude" {
		t.Fatalf("first compose Choose should be the harness pick, got %v", items)
	}
}

// Aborting the harness pick (Esc) ends compose before any mode chooser.
func TestComposeHarnessCancelAborts(t *testing.T) {
	rec := captureComposeLaunch(t)
	f := &queuedFinder{chooseQ: nil} // harness Choose returns ""
	a := App{mux: inactiveMux{}, find: f}
	a.compose(claudeCfg("", ""), nil, nil)

	if rec.called {
		t.Fatal("a cancelled harness pick must not launch")
	}
	if len(f.chooseItems) != 1 {
		t.Fatalf("compose should stop after the harness pick, got %d Choose calls", len(f.chooseItems))
	}
}
