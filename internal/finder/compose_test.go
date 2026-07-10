package finder

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/keys"
)

// c and C both open the ONE compose entry, ending the picker with a Compose
// choice and never a plain New/NewArgs.
func TestComposeKeyYieldsComposeChoice(t *testing.T) {
	for _, key := range []string{"c", "C"} {
		p := &picker{km: keys.Build(nil)}
		done := p.dispatch(p.km.Lookup(key))
		if !done {
			t.Fatalf("%q should end the picker", key)
		}
		if !p.choice.Compose {
			t.Fatalf("%q should set Compose, got %#v", key, p.choice)
		}
		if p.choice.New || p.choice.NewArgs {
			t.Fatalf("%q must not set New/NewArgs, got %#v", key, p.choice)
		}
	}
}

// The multiline editor: runes append, Enter inserts a newline, Backspace drops
// the last rune, Ctrl-D accepts, Esc/Ctrl-C cancels.
func TestMultilineKey(t *testing.T) {
	// Type "ab", newline, "c" -> "ab\nc".
	text := ""
	for _, e := range []ev{{t: evRune, r: 'a'}, {t: evRune, r: 'b'}, {t: evEnter}, {t: evRune, r: 'c'}} {
		next, done, _ := multilineKey(text, e)
		if done {
			t.Fatalf("editing keys must not close the editor")
		}
		text = next
	}
	if text != "ab\nc" {
		t.Fatalf("buffer = %q, want %q", text, "ab\nc")
	}

	// Backspace drops the last rune (here the trailing newline is preserved; drop 'c').
	if next, _, _ := multilineKey("ab\nc", ev{t: evBack}); next != "ab\n" {
		t.Fatalf("backspace buffer = %q, want %q", next, "ab\n")
	}

	// Ctrl-D accepts and returns the current buffer.
	if next, done, ok := multilineKey("done", ev{t: evCtrl, r: 'd'}); !done || !ok || next != "done" {
		t.Fatalf("ctrl-d = (%q,%v,%v), want (\"done\",true,true)", next, done, ok)
	}

	// A non-D Ctrl chord is ignored, editor stays open.
	if _, done, _ := multilineKey("x", ev{t: evCtrl, r: 'a'}); done {
		t.Fatalf("ctrl-a must not close the editor")
	}

	// Esc and Ctrl-C both cancel (done, not ok).
	for _, e := range []ev{{t: evEsc}, {t: evCtrlC}} {
		if _, done, ok := multilineKey("x", e); !done || ok {
			t.Fatalf("cancel key %v = (done=%v ok=%v), want (true,false)", e.t, done, ok)
		}
	}
}
