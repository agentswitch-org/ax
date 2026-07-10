package meta

import "testing"

// TestLoadAllCacheInvalidation pins the freshness contract for the mtime-gated
// LoadAll cache: a later Save (which rewrites a sidecar, bumping the meta dir's
// mtime) must be visible on the next LoadAll, and a Remove must drop the entry.
// Without invalidation the picker/index merge would serve a stale re-tag.
func TestLoadAllCacheInvalidation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Reset the package-level cache so a prior test's state can't leak in.
	loadAll.Lock()
	loadAll.out, loadAll.dir, loadAll.mtime, loadAll.scanned = nil, "", 0, 0
	loadAll.Unlock()

	if err := Save("s1", Meta{Name: "first"}); err != nil {
		t.Fatal(err)
	}
	if got := LoadAll()["s1"].Name; got != "first" {
		t.Fatalf("initial LoadAll name = %q, want first", got)
	}

	// Re-tag: a new Save must be reflected, not served from the warm cache.
	if err := Save("s1", Meta{Name: "second"}); err != nil {
		t.Fatal(err)
	}
	if got := LoadAll()["s1"].Name; got != "second" {
		t.Fatalf("after re-tag LoadAll name = %q, want second (stale cache)", got)
	}

	// A second session added later must appear.
	if err := Save("s2", Meta{Name: "other"}); err != nil {
		t.Fatal(err)
	}
	if got := LoadAll(); got["s2"].Name != "other" || len(got) != 2 {
		t.Fatalf("after add, LoadAll = %v, want 2 entries incl s2", got)
	}

	// Removal must drop it.
	Remove("s2")
	if got := LoadAll(); len(got) != 1 {
		t.Fatalf("after remove, LoadAll has %d entries, want 1", len(got))
	}
}

func TestSetArchivedRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := SetArchived("s1", true); err != nil {
		t.Fatal(err)
	}
	m := Load("s1")
	if !m.Archived || m.ArchivedAt.IsZero() {
		t.Fatalf("SetArchived(true) = %+v", m)
	}
	if err := SetArchived("s1", false); err != nil {
		t.Fatal(err)
	}
	m = Load("s1")
	if m.Archived || !m.ArchivedAt.IsZero() {
		t.Fatalf("SetArchived(false) = %+v", m)
	}
}
