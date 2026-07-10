package remap

import "testing"

func TestAddPersistsAndRepointsChains(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	Add("/old", "/mid")
	Add("/mid", "/new")
	Add("", "/ignored")
	Add("/same", "/same")

	got := Load()
	want := map[string]string{"/old": "/new", "/mid": "/new"}
	if len(got) != len(want) {
		t.Fatalf("Load = %#v, want %#v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("mapping %q = %q, want %q (all %#v)", k, got[k], v, got)
		}
	}
}
