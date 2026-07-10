package view

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/session"
)

// WindowTitle folds in the host (remote) and group (run) prefixes ahead of the
// mux backend's own ax namespace prefix, so a run's windows cluster together
// in the window list even though the picker's own title column is untouched.
func TestWindowTitle(t *testing.T) {
	cases := []struct {
		name string
		s    session.Session
		want string
	}{
		{"local, no group", session.Session{Dir: "/home/x/proj", Harness: "claude"}, "proj·claude"},
		{"trailing slash trimmed", session.Session{Dir: "/home/x/proj/", Harness: "claude"}, "proj·claude"},
		{"remote host prefixed", session.Session{Dir: "/home/x/proj", Harness: "claude", Host: "vm"}, "vm:proj·claude"},
		{"group folded in", session.Session{Dir: "/home/x/proj", Harness: "claude", Group: "run1"}, "run1/proj·claude"},
		{"group and host both fold in", session.Session{Dir: "/home/x/proj", Harness: "claude", Host: "vm", Group: "run1"}, "run1/vm:proj·claude"},
	}
	for _, c := range cases {
		if got := WindowTitle(c.s); got != c.want {
			t.Errorf("%s: WindowTitle = %q, want %q", c.name, got, c.want)
		}
	}
}
