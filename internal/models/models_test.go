package models

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPrefersStateSnapshotAndLookup(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(statePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	want := DB{
		"local-model": {
			Context:    123,
			OutputLim:  45,
			Input:      1.5,
			Output:     2.5,
			CacheRead:  0.25,
			CacheWrite: 0.75,
		},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	db := Load()
	got, ok := db.Lookup("local-model")
	if !ok {
		t.Fatalf("Lookup local-model failed in %#v", db)
	}
	if got != want["local-model"] {
		t.Fatalf("Lookup = %#v, want %#v", got, want["local-model"])
	}
	if _, ok := db.Lookup("missing"); ok {
		t.Fatal("Lookup missing model returned ok")
	}
}

func TestLoadFallsBackWhenStateSnapshotInvalid(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(statePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(), []byte("{bad json"), 0o600); err != nil {
		t.Fatal(err)
	}

	db := Load()
	if len(db) == 0 {
		t.Fatal("Load returned empty DB after invalid state snapshot; want embedded fallback")
	}
	if _, ok := db.Lookup("local-model"); ok {
		t.Fatal("invalid state snapshot leaked into loaded DB")
	}
}
