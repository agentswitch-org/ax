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

// ResolveAliasPrefix resolves a short unambiguous prefix of a launch id to its
// adopted session id. The picker and human-facing output shorten ids, and the
// placeholder session a short launch id would prefix-match against is removed
// at adoption — so without this, the full launch id keeps working (via
// ResolveAlias) while its short form dangles.
func ResolveAliasPrefix(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	entries, err := os.ReadDir(aliasDir())
	if err != nil {
		return "", false
	}
	match := ""
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, id) {
			continue
		}
		if match != "" && match != name {
			return "", false // ambiguous prefix: leave the id alone
		}
		match = name
	}
	if match == "" {
		return "", false
	}
	if to := ResolveAlias(match); to != match {
		return to, true
	}
	return "", false
}

// Aliases returns every launch-id -> session-id forwarding pointer, for views
// that need the reverse join (e.g. `ax list --json` stamping each adopted
// session with the id its launch printed).
func Aliases() map[string]string {
	entries, err := os.ReadDir(aliasDir())
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		from := e.Name()
		if to := ResolveAlias(from); to != from {
			out[from] = to
		}
	}
	return out
}
