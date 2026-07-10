package finder

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

// applyRename persists the new name to the meta sidecar and updates the
// in-memory row immediately, without touching other sessions.
func TestApplyRenamePersistsAndUpdatesInMemory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	p := newTestPicker([]session.Session{
		{ID: "a", Name: "old-name"},
		{ID: "b", Name: "untouched"},
	}, map[string]view.RowMeta{})

	p.applyRename("a", "new-name")

	if p.all[0].Name != "new-name" {
		t.Fatalf("in-memory row a should be renamed, got %q", p.all[0].Name)
	}
	if p.all[1].Name != "untouched" {
		t.Fatalf("row b should be untouched, got %q", p.all[1].Name)
	}
	if got := meta.Load("a").Name; got != "new-name" {
		t.Fatalf("meta sidecar for a should persist the new name, got %q", got)
	}
	if got := meta.Load("b").Name; got != "" {
		t.Fatalf("meta sidecar for b should not be written, got %q", got)
	}
}

// renameRow never acts on a remote row: its sidecar belongs to the owning
// host, so the guard must return before even opening the rename prompt.
func TestRenameRowSkipsRemoteRow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	p := newTestPicker([]session.Session{{ID: "r", Host: "otherhost", Name: "remote-name"}}, map[string]view.RowMeta{})
	p.renameRow() // p.sc is nil; a remote row must bail before touching it
	if p.all[0].Name != "remote-name" {
		t.Fatalf("remote row should not be renamed, got %q", p.all[0].Name)
	}
}
