package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/meta"
)

func TestRunRecipeWritesMetaAndExportsTrackedRootEnv(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("AX_MAX_COST", "12.5")
	t.Setenv("AX_MAX_TOKENS", "1000")
	t.Setenv("AX_MAX_WORKERS", "3")
	t.Setenv("AX_TIMEOUT", "5m")
	writeAppCfg(t, "")
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	dir := t.TempDir()
	recipePath := filepath.Join(t.TempDir(), "smoke.sh")
	if err := os.WriteFile(recipePath, []byte("# ax: name = Smoke\n# ax: description = Prints and exits\necho ok\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	var gotID, gotCmd string
	var gotEnv []string
	orig := execHeldEnvFn
	execHeldEnvFn = func(id, cmd string, env []string) {
		gotID, gotCmd, gotEnv = id, cmd, append([]string(nil), env...)
	}
	t.Cleanup(func() { execHeldEnvFn = orig })

	recipeID, runID, err := (App{mux: inactiveMux{}}).RunRecipe(recipePath, "claude", dir)
	if err != nil {
		t.Fatalf("RunRecipe() error = %v", err)
	}
	if recipeID == "" || runID == "" || gotID != recipeID {
		t.Fatalf("ids: recipeID=%q runID=%q execID=%q", recipeID, runID, gotID)
	}
	if !strings.Contains(gotCmd, "bash") || !strings.Contains(gotCmd, recipePath) {
		t.Fatalf("recipe command = %q, want bash plus recipe path", gotCmd)
	}

	m := meta.Load(recipeID)
	if m.Mode != "recipe" || m.Harness != "recipe" || m.Name != "Smoke" || m.Task != "Prints and exits" {
		t.Fatalf("recipe meta identity = %#v", m)
	}
	if m.Group != runID || m.Parent != "" || m.Origin != "human" || m.Dir != dir {
		t.Fatalf("recipe meta run fields = %#v", m)
	}
	if m.RecipePath != recipePath || len(m.RecipeInterpreter) != 1 || m.RecipeInterpreter[0] != "bash" || m.LogPath == "" {
		t.Fatalf("recipe meta launch fields = %#v", m)
	}

	env := envMap(gotEnv)
	want := map[string]string{
		"AX_SESSION_ID":  recipeID,
		"AX_RECIPE_ID":   recipeID,
		"AX_RUN":         runID,
		"AX_GROUP":       runID,
		"AX_DEPTH":       "-1",
		"AX_MAX_DEPTH":   "1",
		"AX_HARNESS":     "claude",
		"AX_DIR":         dir,
		"AX_RECIPE":      recipePath,
		"AX_RECIPE_DIR":  filepath.Dir(recipePath),
		"AX_RECIPE_NAME": "Smoke",
		"AX_MAX_COST":    "12.5",
		"AX_MAX_TOKENS":  "1000",
		"AX_MAX_WORKERS": "3",
		"AX_TIMEOUT":     "5m0s",
	}
	for k, v := range want {
		if env[k] != v {
			t.Fatalf("env %s = %q, want %q (all env: %#v)", k, env[k], v, env)
		}
	}
	if env["AX"] == "" || !filepath.IsAbs(env["AX"]) {
		t.Fatalf("AX = %q, want absolute ax path", env["AX"])
	}
	if _, ok := env["AX_PARENT"]; ok {
		t.Fatalf("recipe root env must not set AX_PARENT: %#v", env)
	}
}

func TestRunRecipeMuxPathUsesHeldWrapper(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeAppCfg(t, "")
	dir := t.TempDir()
	recipePath := filepath.Join(t.TempDir(), "smoke.sh")
	if err := os.WriteFile(recipePath, []byte("echo ok\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	mx := &recordingActiveMux{}
	recipeID, runID, err := (App{mux: mx}).RunRecipe(recipePath, "pi", dir)
	if err != nil {
		t.Fatalf("RunRecipe() error = %v", err)
	}
	if len(mx.opens) != 1 {
		t.Fatalf("mux opens = %d, want 1", len(mx.opens))
	}
	call := mx.opens[0]
	if call.dir != dir || call.sessionID != recipeID || !call.focus {
		t.Fatalf("mux open call = %#v", call)
	}
	if !strings.Contains(call.title, runID+"/smoke") {
		t.Fatalf("title = %q, want run/name", call.title)
	}
	if !strings.Contains(call.cmd, "AX_RECIPE") || (!strings.Contains(call.cmd, " attach ") && !strings.Contains(call.cmd, " run ")) {
		t.Fatalf("mux command does not preserve recipe env and holder wrapper: %q", call.cmd)
	}
}

type failingActiveMux struct{ recordingActiveMux }

func (m *failingActiveMux) Open(dir, title, cmd, sessionID, target string, focus bool) error {
	return errFailingMux
}

var errFailingMux = fmt.Errorf("mux open failed")

// TestRunRecipeMuxOpenFailureRemovesOrphanMeta verifies that when the launch
// fails after the metadata is written, RunRecipe deletes the metadata so no
// orphan run root lingers in `ax ls`.
func TestRunRecipeMuxOpenFailureRemovesOrphanMeta(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeAppCfg(t, "")
	dir := t.TempDir()
	recipePath := filepath.Join(t.TempDir(), "smoke.sh")
	if err := os.WriteFile(recipePath, []byte("echo ok\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	recipeID, _, err := (App{mux: &failingActiveMux{}}).RunRecipe(recipePath, "claude", dir)
	if err == nil {
		t.Fatal("RunRecipe() error = nil, want mux open failure")
	}
	if recipeID != "" {
		t.Fatalf("recipeID = %q, want empty on failure", recipeID)
	}
	if all := meta.LoadAll(); len(all) != 0 {
		t.Fatalf("meta.LoadAll() = %v, want no orphan session after failed launch", all)
	}
}

// TestRecipeRootIsDepthTransparent verifies the recipe root does not consume a
// depth level: its child computes depth 0, exactly as a human running the same
// `ax <harness> ...` directly would, so the child's own workers (depth 1) are
// still allowed under the default --max-depth 1.
func TestRecipeRootIsDepthTransparent(t *testing.T) {
	env := recipeEnv("id", "run", "claude", "/tmp", "/tmp/r.sh", "r", nil)
	got := envMap(env)
	if got["AX_DEPTH"] != "-1" {
		t.Fatalf("recipe AX_DEPTH = %q, want -1 (depth-transparent root)", got["AX_DEPTH"])
	}
	// The child reads the recipe env and computes its own depth the same way
	// launch()/continue() do: envInt("AX_DEPTH", -1) + 1.
	t.Setenv("AX_DEPTH", got["AX_DEPTH"])
	if childDepth := envInt("AX_DEPTH", -1) + 1; childDepth != 0 {
		t.Fatalf("recipe child depth = %d, want 0 (matches a human launch)", childDepth)
	}
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			out[k] = v
		}
	}
	return out
}
