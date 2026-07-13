package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/recipe"
)

// RunRecipe launches a user-owned recipe script as a tracked run root. Ax does
// not parse or rewrite the script; it only wraps the script process so the root
// and any nested ax children share one run tree.
func (a App) RunRecipe(path, harness, dir string) (recipeID, runID string, err error) {
	return a.launchRecipe(path, harness, dir)
}

func (a App) launchRecipe(path, harness, dir string) (recipeID, runID string, err error) {
	cfg, _ := config.Load()
	recipePath, err := absExpanded(path)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(recipePath)
	if err != nil {
		return "", "", fmt.Errorf("recipe %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("recipe %s: not a regular file", path)
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	chosenDir, err := absExpanded(dir)
	if err != nil {
		return "", "", err
	}
	interp, err := recipe.Interpreter(recipePath)
	if err != nil {
		return "", "", err
	}
	name, description := recipeNameDescription(recipePath)
	task := strings.TrimSpace(description)
	if task == "" {
		task = recipePath
	}

	recipeID, runID = newUUID(), shortID()
	labels := seedProjectLabel(nil, chosenDir)
	logPath := recipeLogPath(recipeID)
	if err := meta.Save(recipeID, meta.Meta{
		Name: name, Task: task, Group: runID, Parent: "", Origin: "human",
		Harness: "recipe", Dir: chosenDir, Labels: labels, Mode: "recipe",
		RecipePath: recipePath, RecipeInterpreter: append([]string(nil), interp...), LogPath: logPath,
	}); err != nil {
		return "", "", err
	}

	cmd := recipeCommand(interp, recipePath)
	env := recipeEnv(recipeID, runID, harness, chosenDir, recipePath, name, labels)
	title := launchWindowTitle(name, "recipe", runID)
	if a.mux != nil && a.mux.Active() {
		target := muxTargetFor(labels, cfg.MuxGroup)
		// The wrapper opens in the BACKGROUND (focus=false). It is a bootstrap: the
		// script it holds launches the human-facing session (a coordinator's nested
		// `ax <harness> ... --attach`), and that --attach launch is what grabs the
		// foreground so the human lands inside the running harness. If the wrapper
		// grabbed the foreground here too, both it and the coordinator would open in
		// the foreground and the human's landing spot would hinge on the coordinator's
		// focus switch WINNING A RACE against the wrapper's: when it loses (or the
		// wrapper's foreground switch sticks, as it does from a picker popup / across a
		// grouped session), the human is left on the wrapper's held shell instead of
		// the coordinator. Backgrounding the wrapper removes that race: the coordinator
		// is the sole foreground switch, so the human deterministically lands in it.
		// The window is still opened and tracked; it is just not focused, and the human
		// reaches it (if they want) through the picker like any other background run.
		if err := a.mux.Open(chosenDir, title, axEnvPrefix(env)+heldWindowCmd(recipeID, cmd), recipeID, target, false); err != nil {
			// The launch never took, so don't leave a metadata-only orphan session
			// (which would show up in `ax ls` as a run that never started).
			meta.Remove(recipeID)
			return "", "", err
		}
		return recipeID, runID, nil
	}
	if err := os.Chdir(chosenDir); err != nil {
		meta.Remove(recipeID)
		return "", "", err
	}
	execHeldEnvFn(recipeID, cmd, env)
	return recipeID, runID, nil
}

func absExpanded(path string) (string, error) {
	path = config.ExpandHome(path)
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Abs(path)
}

func recipeNameDescription(path string) (name, description string) {
	base := filepath.Base(path)
	name = strings.TrimSuffix(base, filepath.Ext(base))
	if name == "" {
		name = base
	}
	if data, err := os.ReadFile(path); err == nil {
		headerName, headerDescription := recipe.ParseHeader(data)
		if strings.TrimSpace(headerName) != "" {
			name = strings.TrimSpace(headerName)
		}
		description = strings.TrimSpace(headerDescription)
	}
	return name, description
}

func recipeLogPath(id string) string {
	return filepath.Join(axdir.State("recipes"), id+".log")
}

func recipeCommand(interp []string, path string) string {
	argv := append(append([]string(nil), interp...), path)
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func recipeEnv(id, runID, harness, dir, path, name string, labels []string) []string {
	recipeDir := filepath.Dir(path)
	// The recipe root is depth-transparent: it seeds AX_DEPTH=-1 so its first
	// child computes depth 0 (envInt("AX_DEPTH", -1)+1), exactly as if a human
	// had run that same `ax <harness> ...` directly. Seeding depth 0 here would
	// instead push the child to depth 1, silently consuming a level and refusing
	// the child's own workers under the default --max-depth 1.
	env := axEnv(id, runID, "", -1, 1, envFences(), labels)
	env = append(env,
		"AX="+selfAbs(),
		"AX_RECIPE="+path,
		"AX_RECIPE_DIR="+recipeDir,
		"AX_RECIPE_NAME="+name,
		"AX_RECIPE_ID="+id,
		"AX_HARNESS="+harness,
		"AX_DIR="+dir,
	)
	return env
}

func selfAbs() string {
	p := self()
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if lp, err := exec.LookPath(p); err == nil {
		if abs, err := filepath.Abs(lp); err == nil {
			return abs
		}
		return lp
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
