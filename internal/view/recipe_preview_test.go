package view

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/models"
	"github.com/agentswitch-org/ax/internal/session"
)

func TestRecipePreviewShowsLogTail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "recipe.log")
	var lines []string
	for i := 0; i < 90; i++ {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := session.Session{
		ID: "r1", Harness: "recipe", Mode: "recipe", Name: "Recipe", Task: "do it",
		Group: "run-1", Origin: "human", Dir: dir, RecipePath: "/tmp/r.sh",
		RecipeInterpreter: []string{"bash"}, LogPath: logPath,
	}

	got := Preview(config.Config{}, models.DB{}, s)
	if !strings.Contains(got, "recipe: /tmp/r.sh") || !strings.Contains(got, "interpreter: bash") {
		t.Fatalf("recipe metadata missing from preview:\n%s", got)
	}
	if strings.Contains(got, "line 00") {
		t.Fatalf("preview included the beginning of a long log, want tail only:\n%s", got)
	}
	if !strings.Contains(got, "line 89") {
		t.Fatalf("preview missing log tail:\n%s", got)
	}
}

func TestRecipePreviewNoOutputYet(t *testing.T) {
	s := session.Session{Harness: "recipe", Mode: "recipe", Name: "Recipe", Dir: t.TempDir()}
	got := Preview(config.Config{}, models.DB{}, s)
	if !strings.Contains(got, "no recipe output yet") {
		t.Fatalf("preview = %q, want no-output placeholder", got)
	}
}
