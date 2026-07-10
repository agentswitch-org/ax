package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodingProjectRecipeCapsWorkers(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "recipes", "coding-project-"+"coor"+"dinator")
	for _, name := range []string{"coor" + "dinator.sh", "coor" + "dinator.ps1"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "--max-workers 2") {
			t.Fatalf("%s must pass --max-workers 2", name)
		}
	}
}
