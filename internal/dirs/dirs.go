// Package dirs provides candidate directories for starting a new session.
// Source is the contract; zoxide is the current implementation. Swap it by
// returning a different Source from New.
package dirs

import (
	"os/exec"
	"strings"
)

// Source lists candidate directories, most relevant first.
type Source interface {
	Candidates() []string
}

// New returns the configured directory source (zoxide today).
func New() Source { return zoxide{} }

type zoxide struct{}

func (zoxide) Candidates() []string {
	out, err := exec.Command("zoxide", "query", "-l").Output()
	if err != nil {
		return nil
	}
	var dirs []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			dirs = append(dirs, l)
		}
	}
	return dirs
}
