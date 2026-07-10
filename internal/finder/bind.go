package finder

import (
	"os"
	"strings"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/shell"
)

// handleBindKey resolves the key pressed after the leader (keys.Bind) against
// the user's [[bind]] table and runs the first match. Any other key, Esc
// included, just leaves leader mode with no action: a mistyped chord is a
// no-op rather than accidentally firing the wrong command.
func (p *picker) handleBindKey(e ev) bool {
	p.awaitingBind = false
	p.previewDirty = true
	if e.t == evCtrlC {
		return true
	}
	if b, ok := findBind(p.cfg.Binds, evToKey(e)); ok {
		return p.runBind(b)
	}
	return false
}

// findBind looks up the [[bind]] whose key normalizes (keys.NormKey, the same
// as the built-in keymap) to the given pressed key.
func findBind(binds []config.Bind, key string) (config.Bind, bool) {
	for _, b := range binds {
		if b.Key != "" && keys.NormKey(b.Key) == key {
			return b, true
		}
	}
	return config.Bind{}, false
}

// runBind expands a keybinding's command template against the current
// selection and runs it in the shell, suspending the TUI so the command owns
// the terminal (an editor, a pager, an interactive recipe) and restoring it
// once the command exits. ax just runs the command; what it does is entirely
// up to the template. Reports whether the picker must end: only true if the
// tty could not be reopened afterward, in which case there is nothing left to
// render into.
func (p *picker) runBind(b config.Bind) bool {
	s, _ := p.cur() // the zero Session when nothing is selected; placeholders expand to ""
	cmd := expandBind(b.Run, s, b.File)
	if strings.TrimSpace(cmd) == "" {
		return false
	}
	p.sc.suspend()
	c := shell.Command(cmd)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Run()
	if err := p.sc.resume(); err != nil {
		return true
	}
	p.previewDirty = true
	return false
}

// expandBind fills a [[bind]] command template with the selected session's
// context ({id} {run} {dir} {transcript}) and the binding's own fixed {file},
// shell-quoting each value (the same technique internal/notify uses for its
// templates) so a path or id with spaces or quotes cannot break out of the
// substitution.
func expandBind(tpl string, s session.Session, file string) string {
	file = config.ExpandHome(file)
	return strings.NewReplacer(
		"{id}", shell.QuotePosix(s.ID),
		"{run}", shell.QuotePosix(s.Group),
		"{dir}", shell.QuotePosix(s.Dir),
		"{transcript}", shell.QuotePosix(s.File),
		"{file}", shell.QuotePosix(file),
	).Replace(tpl)
}
