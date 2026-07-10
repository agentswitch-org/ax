package session

import (
	"path/filepath"
	"testing"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
)

func TestIndexKeepsRecipeMetaRowsWhileLiveAndAfterConclusion(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := "recipe-root"
	if err := meta.Save(id, meta.Meta{
		Harness: "recipe", Mode: "recipe", Name: "Smoke", Task: "/tmp/recipe.sh",
		Group: "run-1", Origin: "human", Dir: t.TempDir(),
		RecipePath: "/tmp/recipe.sh", RecipeInterpreter: []string{"bash"}, LogPath: "/tmp/recipe.log",
	}); err != nil {
		t.Fatal(err)
	}
	if got := findIndexedRecipe(config.Config{}, id); got != nil {
		t.Fatalf("recipe without live heartbeat or terminal hook was indexed: %#v", *got)
	}

	writeLegacyLive(t, id, "bash /tmp/recipe.sh")
	liveRow := findIndexedRecipe(config.Config{}, id)
	if liveRow == nil {
		t.Fatal("live recipe meta row was not indexed")
	}
	if liveRow.Group != "run-1" || liveRow.Parent != "" || liveRow.RecipePath != "/tmp/recipe.sh" || liveRow.LogPath != "/tmp/recipe.log" {
		t.Fatalf("live recipe row metadata = %#v", *liveRow)
	}

	live.Remove(id)
	if err := axdir.WriteFileAtomic(filepath.Join(axdir.State("hookstate"), id), []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
	doneRow := findIndexedRecipe(config.Config{}, id)
	if doneRow == nil {
		t.Fatal("terminal recipe meta row was not indexed after live heartbeat removal")
	}
	if doneRow.Mode != "recipe" || doneRow.Harness != "recipe" {
		t.Fatalf("terminal recipe row identity = %#v", *doneRow)
	}
}

func findIndexedRecipe(cfg config.Config, id string) *Session {
	for _, s := range Index(cfg) {
		if s.ID == id {
			return &s
		}
	}
	return nil
}
