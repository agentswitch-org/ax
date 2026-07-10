package meta

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/agentswitch-org/ax/internal/axdir"
)

// Aliases map the session id a launch printed to the id the harness actually
// created. A harness with no {newid} slot (codex, opencode) mints its own id
// after launch; the run wrapper's adopt step migrates the heartbeat and meta
// sidecar to the real id, and without a forwarding pointer the id the caller
// captured at launch would dangle: `ax read/wait/result/send` on it would find
// nothing, ever. One file per alias at $XDG_STATE_HOME/ax/alias/<launch-id>,
// containing the real id, so a caller holding only the printed id keeps a
// working handle for the session's whole life.

func aliasDir() string { return axdir.State("alias") }

// SaveAlias records that queries for id `from` should resolve to session `to`.
// Written by the adopt step the moment the real session id is discovered.
func SaveAlias(from, to string) error {
	if from == "" || to == "" || from == to {
		return nil
	}
	return axdir.WriteFileAtomic(filepath.Join(aliasDir(), from), []byte(to), 0o600)
}

// ResolveAlias returns the session id queries for id should use: the adopted
// real id when a launch-time alias exists, else id unchanged. One hop only (an
// alias always points at a real session, never another alias).
func ResolveAlias(id string) string {
	if id == "" {
		return id
	}
	data, err := os.ReadFile(filepath.Join(aliasDir(), id))
	if err != nil {
		return id
	}
	if to := strings.TrimSpace(string(data)); to != "" {
		return to
	}
	return id
}

// RemoveAlias deletes a launch id's forwarding pointer (best-effort teardown).
func RemoveAlias(id string) { os.Remove(filepath.Join(aliasDir(), id)) }
