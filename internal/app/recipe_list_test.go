package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/wire"
)

// A tracked recipe root has no harness transcript, so it surfaces in `ax list
// --json` purely from its metadata sidecar: while it holds a live heartbeat,
// and again after it concludes (the durable terminal-hook marker). This is the
// end-to-end list-projection twin of session.Index's metaOnlyVisible: it proves
// the recipe run root is watchable as a unit through the wire report the picker
// and remote federation read, both live and after conclusion.
func TestListJSONShowsRecipeRootLiveAndAfterConclusion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads USERPROFILE on Windows
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", home)
	cfgPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfgPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", cfgPath) // no hosts: default list stays local-only

	id, group := "recipe-root", "run-list-1"
	if err := meta.Save(id, meta.Meta{
		Harness: "recipe", Mode: "recipe", Name: "Smoke", Task: "prints ok",
		Group: group, Origin: "human", Dir: t.TempDir(),
		RecipePath: "/tmp/recipe.sh", RecipeInterpreter: []string{"bash"}, LogPath: "/tmp/recipe.log",
	}); err != nil {
		t.Fatal(err)
	}

	find := func(t *testing.T) *wire.Session {
		t.Helper()
		out := captureStdout(t, func() { App{mux: inactiveMux{}}.List([]string{"--json"}) })
		var rep wire.Report
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("list --json is not a wire.Report: %v\n%s", err, out)
		}
		for i := range rep.Sessions {
			if rep.Sessions[i].ID == id {
				return &rep.Sessions[i]
			}
		}
		return nil
	}

	// With neither a heartbeat nor a terminal marker the meta-only row is invisible.
	if row := find(t); row != nil {
		t.Fatalf("recipe root visible with no heartbeat and no terminal marker: %#v", *row)
	}

	// Live: a fresh heartbeat surfaces the row; mode=recipe and the run group carry.
	writeLegacyLive(t, id, "bash /tmp/recipe.sh")
	row := find(t)
	if row == nil {
		t.Fatal("live recipe root missing from list --json")
	}
	if row.Mode != "recipe" || row.Group != group || row.Name != "Smoke" {
		t.Fatalf("live recipe row = %#v", *row)
	}

	// Concluded: the heartbeat is gone but the durable terminal-hook marker keeps
	// the recipe root in the report, so a concluded run is still listable.
	live.Remove(id)
	if err := axdir.WriteFileAtomic(filepath.Join(axdir.State("hookstate"), id), []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
	if row := find(t); row == nil {
		t.Fatal("concluded recipe root dropped from list --json")
	} else if row.Mode != "recipe" || row.Group != group {
		t.Fatalf("concluded recipe row = %#v", *row)
	}
}
