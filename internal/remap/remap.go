// Package remap persists user-confirmed directory relinks so sessions whose
// recorded project folder was renamed or moved still resolve to the live path.
// A single JSON file under ~/.local/state/ax maps an old directory to its new
// location; the index applies it so every session in a moved folder relinks at
// once.
package remap

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/agentswitch-org/ax/internal/axdir"
)

func path() string {
	return axdir.StatePath("dirmap.json")
}

// Load returns the old->new directory map (empty if none recorded).
func Load() map[string]string {
	m := map[string]string{}
	if data, err := os.ReadFile(path()); err == nil {
		json.Unmarshal(data, &m)
	}
	return m
}

// Add records old->new and persists it. Any existing mapping that pointed at
// old is repointed to new, so a folder moved twice resolves straight to its
// final location instead of forming a chain.
func Add(old, new string) {
	if old == "" || old == new {
		return
	}
	m := Load()
	for k, v := range m {
		if v == old {
			m[k] = new
		}
	}
	m[old] = new
	save(m)
}

func save(m map[string]string) {
	p := path()
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	if data, err := json.MarshalIndent(m, "", "  "); err == nil {
		os.WriteFile(p, data, 0o600)
	}
}
