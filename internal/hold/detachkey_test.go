package hold

import "testing"

// detach_prefix parsing: ctrl-<letter> and the caret form map to control
// bytes, anything else (including empty) falls back to the ctrl-a default.
func TestParseDetachKey(t *testing.T) {
	cases := []struct {
		in   string
		want byte
		ok   bool
	}{
		{"ctrl-q", 0x11, true},
		{"CTRL-Q", 0x11, true},
		{" ctrl-g ", 0x07, true},
		{"^q", 0x11, true},
		{"^A", 0x01, true},
		{"ctrl-z", 0x1a, true},
		{"", 0, false},
		{"q", 0, false},
		{"ctrl-", 0, false},
		{"ctrl-1", 0, false},
		{"ctrl-qq", 0, false},
		{"^", 0, false},
		{"^\\", 0, false}, // only letters: the fallback 0x1c is always on anyway
	}
	for _, c := range cases {
		got, ok := ParseDetachKey(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseDetachKey(%q) = %#x, %v; want %#x, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// detach_key parsing: a bare letter is the chord's command letter; the
// pre-chord ctrl-/caret forms are accepted as their letter so old configs
// keep detaching.
func TestParseDetachLetter(t *testing.T) {
	cases := []struct {
		in   string
		want byte
		ok   bool
	}{
		{"d", 'd', true},
		{"D", 'd', true},
		{" q ", 'q', true},
		{"ctrl-g", 'g', true},
		{"^p", 'p', true},
		{"", 0, false},
		{"dd", 0, false},
		{"1", 0, false},
		{"ctrl-", 0, false},
		{"^", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseDetachLetter(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseDetachLetter(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestDetachBytesAndLabel(t *testing.T) {
	if DefaultDetachPrefix != 0x01 {
		t.Errorf("DefaultDetachPrefix = %#x, want ctrl-a (0x01)", DefaultDetachPrefix)
	}
	if DefaultDetachLetter != 'd' {
		t.Errorf("DefaultDetachLetter = %q, want d", DefaultDetachLetter)
	}
	if b := DetachPrefixByte(""); b != DefaultDetachPrefix {
		t.Errorf("DetachPrefixByte(\"\") = %#x, want the ctrl-a default %#x", b, DefaultDetachPrefix)
	}
	if b := DetachPrefixByte("bogus"); b != DefaultDetachPrefix {
		t.Errorf("DetachPrefixByte(\"bogus\") = %#x, want the ctrl-a default %#x", b, DefaultDetachPrefix)
	}
	if b := DetachPrefixByte("ctrl-b"); b != 0x02 {
		t.Errorf("DetachPrefixByte(\"ctrl-b\") = %#x, want 0x02", b)
	}
	if b := DetachLetterByte(""); b != 'd' {
		t.Errorf("DetachLetterByte(\"\") = %q, want d", b)
	}
	if b := DetachLetterByte("bogus"); b != 'd' {
		t.Errorf("DetachLetterByte(\"bogus\") = %q, want d", b)
	}
	if b := DetachLetterByte("q"); b != 'q' {
		t.Errorf("DetachLetterByte(\"q\") = %q, want q", b)
	}
	if l := DetachLabel("", ""); l != "Ctrl-A then d" {
		t.Errorf("DetachLabel(\"\", \"\") = %q, want Ctrl-A then d", l)
	}
	if l := DetachLabel("^b", "x"); l != "Ctrl-B then x" {
		t.Errorf("DetachLabel(\"^b\", \"x\") = %q, want Ctrl-B then x", l)
	}
}

// The menu chord shares the detach prefix and the bare-letter syntax, and
// defaults to a; its label reads through the shared prefix.
func TestMenuBytesAndLabel(t *testing.T) {
	if DefaultMenuLetter != 'a' {
		t.Errorf("DefaultMenuLetter = %q, want a", DefaultMenuLetter)
	}
	if b := MenuLetterByte(""); b != 'a' {
		t.Errorf("MenuLetterByte(\"\") = %q, want a", b)
	}
	if b := MenuLetterByte("bogus"); b != 'a' {
		t.Errorf("MenuLetterByte(\"bogus\") = %q, want a", b)
	}
	if b := MenuLetterByte("m"); b != 'm' {
		t.Errorf("MenuLetterByte(\"m\") = %q, want m", b)
	}
	if l := MenuLabel("", ""); l != "Ctrl-A then a" {
		t.Errorf("MenuLabel(\"\", \"\") = %q, want Ctrl-A then a", l)
	}
	if l := MenuLabel("^b", "m"); l != "Ctrl-B then m" {
		t.Errorf("MenuLabel(\"^b\", \"m\") = %q, want Ctrl-B then m", l)
	}
}
