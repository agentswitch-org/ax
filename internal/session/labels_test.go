package session

import (
	"reflect"
	"testing"
)

func TestLabelValue(t *testing.T) {
	labels := []string{"pinned", "workstream=blog", "area=lan302"}
	if v := LabelValue(labels, "workstream"); v != "blog" {
		t.Fatalf("workstream = %q", v)
	}
	if v := LabelValue(labels, "area"); v != "lan302" {
		t.Fatalf("area = %q", v)
	}
	if v := LabelValue(labels, "pinned"); v != "" {
		t.Fatalf("bare label should have no value, got %q", v)
	}
	if v := LabelValue(labels, "missing"); v != "" {
		t.Fatalf("missing key should be empty, got %q", v)
	}
}

func TestLabelKeys(t *testing.T) {
	ss := []Session{
		{Labels: []string{"workstream=a", "pinned"}},
		{Labels: []string{"area=x", "workstream=b"}},
	}
	got := LabelKeys(ss)
	want := []string{"workstream", "area"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestEditLabels(t *testing.T) {
	cases := []struct {
		in   []string
		edit string
		want []string
	}{
		{nil, "workstream=hd", []string{"workstream=hd"}},                             // set
		{[]string{"workstream=hd"}, "workstream=video", []string{"workstream=video"}}, // replace (one value per key)
		{[]string{"workstream=hd", "pinned"}, "-workstream", []string{"pinned"}},      // remove by key
		{[]string{"workstream=hd", "pinned"}, "-pinned", []string{"workstream=hd"}},   // remove bare
		{[]string{"workstream=hd"}, "workstream=", nil},                               // "key=" clears
		{[]string{"pinned"}, "pinned", []string{"pinned"}},                            // add flag is idempotent
		{[]string{"a=1"}, "  b = 2 ", []string{"a=1", "b=2"}},                         // whitespace trimmed
		{[]string{"a=1"}, "", []string{"a=1"}},                                        // empty edit no-op
	}
	for _, c := range cases {
		got := EditLabels(c.in, c.edit)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("EditLabels(%v, %q) = %v, want %v", c.in, c.edit, got, c.want)
		}
	}
}

func TestHasLabel(t *testing.T) {
	labels := []string{"pinned", "workstream=blog"}
	for _, want := range []string{"pinned", "workstream=blog", "workstream"} {
		if !HasLabel(labels, want) {
			t.Fatalf("HasLabel(%q) = false", want)
		}
	}
	for _, not := range []string{"workstream=other", "area", "blog"} {
		if HasLabel(labels, not) {
			t.Fatalf("HasLabel(%q) = true", not)
		}
	}
}

// Re-tagging a key via the CLI fold must replace, not accumulate (the bug the
// old app.mergeLabels had).
func TestEditLabelsReplacesOnAdd(t *testing.T) {
	got := EditLabels([]string{"ws=a"}, "ws=b")
	if len(got) != 1 || got[0] != "ws=b" {
		t.Fatalf("re-tag = %v, want [ws=b]", got)
	}
}
