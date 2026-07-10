//go:build unix

package hold

import (
	"bytes"
	"testing"
)

// The chord state machine over single reads: prefix-letter detaches with
// neither byte forwarded, prefix-menu asks for the picker, prefix-prefix
// forwards exactly one literal prefix, prefix-other forwards both bytes
// lossless, and the ctrl-\ fallback detaches anywhere. Bytes after a chord in
// the same read are dropped.
func TestScanChord(t *testing.T) {
	const pfx, cmd, menu = 0x01, 'd', 'a'
	cases := []struct {
		name    string
		in      []byte
		fwd     []byte
		act     chordAction
		pending bool
	}{
		{"plain flows through", []byte("hello"), []byte("hello"), chordNone, false},
		{"chord detaches", []byte{pfx, cmd}, nil, chordDetach, false},
		{"menu chord opens picker", []byte{pfx, menu}, nil, chordMenu, false},
		{"bytes before the chord forward", []byte{'h', 'i', pfx, cmd}, []byte("hi"), chordDetach, false},
		{"bytes before the menu chord forward", []byte{'h', 'i', pfx, menu}, []byte("hi"), chordMenu, false},
		{"bytes after a detach drop", []byte{pfx, cmd, 'z'}, nil, chordDetach, false},
		{"bytes after a menu chord drop", []byte{pfx, menu, 'z'}, nil, chordMenu, false},
		{"double prefix sends one literal", []byte{pfx, pfx}, []byte{pfx}, chordNone, false},
		{"double prefix then letter forwards", []byte{pfx, pfx, cmd}, []byte{pfx, cmd}, chordNone, false},
		{"double prefix then menu letter forwards", []byte{pfx, pfx, menu}, []byte{pfx, menu}, chordNone, false},
		{"prefix-other forwards both", []byte{pfx, 'x'}, []byte{pfx, 'x'}, chordNone, false},
		{"bare d is a plain byte", []byte{cmd}, []byte{cmd}, chordNone, false},
		{"bare a is a plain byte", []byte{menu}, []byte{menu}, chordNone, false},
		{"trailing prefix arms pending", []byte{'a', pfx}, []byte("a"), chordNone, true},
		{"lone prefix arms pending", []byte{pfx}, nil, chordNone, true},
		{"fallback detaches", []byte{'y', 'o', 0x1c}, []byte("yo"), chordDetach, false},
		{"fallback while armed detaches", []byte{pfx, 0x1c}, nil, chordDetach, false},
	}
	for _, c := range cases {
		fwd, act, pending := scanChord(c.in, pfx, cmd, menu, false)
		if !bytes.Equal(fwd, c.fwd) || act != c.act || pending != c.pending {
			t.Errorf("%s: scanChord(%q) = (%q, %v, %v); want (%q, %v, %v)",
				c.name, c.in, fwd, act, pending, c.fwd, c.act, c.pending)
		}
	}
}

// The armed state carries across reads: a prefix ending one read chords with
// the letter (detach) or the menu letter (picker) starting the next, doubles
// into a literal, or releases as prefix-plus-byte.
func TestScanChordAcrossReads(t *testing.T) {
	const pfx, cmd, menu = 0x01, 'd', 'a'

	fwd, act, pending := scanChord([]byte{cmd}, pfx, cmd, menu, true)
	if fwd != nil || act != chordDetach || pending {
		t.Errorf("armed then letter = (%q, %v, %v); want (nil, detach, false)", fwd, act, pending)
	}
	fwd, act, pending = scanChord([]byte{menu}, pfx, cmd, menu, true)
	if fwd != nil || act != chordMenu || pending {
		t.Errorf("armed then menu letter = (%q, %v, %v); want (nil, menu, false)", fwd, act, pending)
	}
	fwd, act, pending = scanChord([]byte{pfx}, pfx, cmd, menu, true)
	if !bytes.Equal(fwd, []byte{pfx}) || act != chordNone || pending {
		t.Errorf("armed then prefix = (%q, %v, %v); want (one literal prefix, none, false)", fwd, act, pending)
	}
	fwd, act, pending = scanChord([]byte{'x'}, pfx, cmd, menu, true)
	if !bytes.Equal(fwd, []byte{pfx, 'x'}) || act != chordNone || pending {
		t.Errorf("armed then other = (%q, %v, %v); want (prefix+x, none, false)", fwd, act, pending)
	}
}
