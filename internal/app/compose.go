package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/recipe"
	"github.com/agentswitch-org/ax/internal/view"
)

// Compose modes: c/C open ONE entry that picks a harness, then one of these
// three shapes, then a directory, then launches. Plain-new is simply the first
// choice, so today's quick-new stays one extra keystroke away.
const (
	composeModePlain    = "plain: a plain harness session"
	composeModeBehavior = "behavior + prompt: a oneshot with a behavior and task"
	composeModeRecipe   = "recipe: run a user-owned recipe script"
)

// The behavior step's two non-file choices: type instruction text inline, or
// launch with no behavior at all. Listed after the behaviors_dir files.
const (
	behaviorInline = "＋ inline behavior text…"
	behaviorSkip   = "∅ skip (no behavior)"
)

// compose is the c/C launch entrypoint: pick a machine (reusing the new-session
// target choice), then a harness, then a mode, then that mode's inputs and a
// directory, then launch. Remote compose is not supported yet; aborting is safer
// than silently discarding the composed intent into a plain remote-new launch.
func (a App) compose(cfg config.Config, hosts []view.HostStatus, hostByName map[string]config.Host) {
	target, ok := a.chooseTarget(hosts)
	if !ok {
		return
	}
	if target != "" {
		fmt.Fprintf(os.Stderr, "ax: remote compose is not yet supported for %s; choose local or run ax new on that host\n", target)
		return
	}

	harness, ok := a.chooseHarness(cfg)
	if !ok {
		return
	}

	mode, _, err := a.find.Choose("mode ❯ ", "compose a launch for "+harness,
		[]string{composeModePlain, composeModeBehavior, composeModeRecipe}, nil)
	if err != nil || mode == "" {
		return
	}
	switch mode {
	case composeModePlain:
		a.New(harness, false, nil)
	case composeModeBehavior:
		a.composeBehavior(cfg, harness)
	case composeModeRecipe:
		a.composeRecipe(cfg, harness)
	}
}

// chooseHarness presents the configured harnesses and returns the picked name.
// ok is false when the user aborts or the config has no harnesses.
func (a App) chooseHarness(cfg config.Config) (string, bool) {
	var names []string
	for _, h := range cfg.Harnesses {
		names = append(names, h.Name)
	}
	name, _, err := a.find.Choose("", "harness", names, nil)
	if err != nil || name == "" {
		return "", false
	}
	return name, true
}

// composeBehavior runs MODE 2: pick a behavior (a file/folder from
// behaviors_dir, inline text, or skip), enter a multiline task prompt, pick a
// directory, then launch `ax <harness> [--behavior PATH|--behavior-text TEXT]
// --dir DIR "<prompt>"`. The prompt is carried in-process (no stdin round-trip);
// runLaunch resolves a file behavior and hard-errors a missing path.
func (a App) composeBehavior(cfg config.Config, harness string) {
	behaviorPath, behaviorText, ok := a.pickBehavior(cfg)
	if !ok {
		return
	}
	prompt, ok := a.find.PromptMultiline("task", "the task prompt for "+harness, "")
	if !ok {
		return
	}
	dir, ok := a.pickDir(cfg, harness)
	if !ok {
		return
	}
	o := launchOpts{
		task:         prompt,
		behavior:     behaviorPath,
		behaviorText: behaviorText,
		dir:          dir,
	}
	composeLaunchFn(a, harness, o)
}

// composeLaunchFn is the seam MODE 2 launches through: `ax <harness>
// [--behavior|--behavior-text] --dir <dir> "<prompt>"` built as launchOpts and
// handed to runLaunch. Overridable in tests to capture the composed flags
// without driving a real harness launch.
var composeLaunchFn = func(a App, harness string, o launchOpts) {
	a.runLaunch(harness, o, launchCtx{})
}

// pickBehavior is the behavior step: the top-level entries of behaviors_dir
// (files and folders, both valid --behavior targets), then an inline-text option
// and a skip option. When behaviors_dir is unset or empty the file list is
// simply empty and only inline/skip are offered. It returns the resolved absolute
// behavior path (file/folder) OR inline behavior text; both empty means skip.
func (a App) pickBehavior(cfg config.Config) (behaviorPath, behaviorText string, ok bool) {
	var items []string
	dir := config.ExpandHome(cfg.BehaviorsDir)
	if cfg.BehaviorsDir != "" {
		items = append(items, behaviorEntries(dir)...)
	}
	items = append(items, behaviorInline, behaviorSkip)

	sel, _, err := a.find.Choose("behavior ❯ ", "pick a behavior, type one inline, or skip", items, nil)
	if err != nil || sel == "" {
		return "", "", false
	}
	switch sel {
	case behaviorSkip:
		return "", "", true
	case behaviorInline:
		text, ok := a.find.PromptMultiline("behavior", "inline behavior (instruction text)", "")
		if !ok {
			return "", "", false
		}
		return "", text, true
	default:
		return filepath.Join(dir, sel), "", true
	}
}

// behaviorEntries lists the top-level entries of a behaviors_dir, files and
// directories alike (a folder of instruction files is a valid --behavior
// target). Dotfiles and editor backups (~ suffix) are skipped, matching the
// recipe listing. Returns nil when the directory is unset, missing, or empty.
func behaviorEntries(dir string) []string {
	if dir == "" {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		name := e.Name()
		if name == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// composeRecipe runs MODE 3: pick a recipe from recipes_dir, pick a directory,
// then run it tracked via RunRecipe with the picked harness exported as the
// AX_HARNESS default. An unset recipes_dir (or an empty/unreadable one) shows a
// clear message and returns cleanly.
func (a App) composeRecipe(cfg config.Config, harness string) {
	if strings.TrimSpace(cfg.RecipesDir) == "" {
		a.notify("no recipes_dir configured. set recipes_dir in your ax config to run recipes")
		return
	}
	recipes, err := recipe.List(config.ExpandHome(cfg.RecipesDir))
	if err != nil {
		a.notify("could not read recipes_dir: " + err.Error())
		return
	}
	if len(recipes) == 0 {
		a.notify("no recipes found in " + cfg.RecipesDir)
		return
	}

	labels := make([]string, len(recipes))
	for i, r := range recipes {
		labels[i] = recipeLabel(r)
	}
	sel, _, err := a.find.Choose("recipe ❯ ", "pick a recipe to run", labels, nil)
	if err != nil || sel == "" {
		return
	}
	idx := -1
	for i, l := range labels {
		if l == sel {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	r := recipes[idx]

	dir, ok := a.pickDir(cfg, r.Name)
	if !ok {
		return
	}
	if _, _, err := a.RunRecipe(r.Path, harness, dir); err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
	}
}

// recipeLabel is a recipe's picker row: its name, and its description after an em
// separator when the header carried one.
func recipeLabel(r recipe.Recipe) string {
	if strings.TrimSpace(r.Description) != "" {
		return r.Name + " · " + r.Description
	}
	return r.Name
}

// notify shows a one-off informational message as a modal (a single dismiss
// choice), so a compose dead-end (no recipes_dir, empty dir) reads clearly on the
// TUI screen instead of vanishing to stderr under the picker.
func (a App) notify(msg string) {
	a.find.Choose("", msg, []string{"ok"}, nil)
}
