// Package axdir is the single home of ax's on-disk state: the state-directory
// resolution every store shares, and the sidecar I/O primitives (0700 dirs,
// atomic JSON writes). One copy here means a permissions or path change reaches
// every store at once instead of being repeated per package.
package axdir

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// StatePath returns the path under $XDG_STATE_HOME/ax without creating it. Use
// it for read-only checks that must not mutate state.
func StatePath(sub ...string) string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(append([]string{base, "ax"}, sub...)...)
}

// State returns $XDG_STATE_HOME/ax joined with sub (e.g. State("meta")),
// creating it 0700 and keeping the ax root itself 0700, since the sidecars
// under it carry tasks, questions, and run telemetry.
func State(sub ...string) string {
	root := StatePath()
	d := filepath.Join(append([]string{root}, sub...)...)
	if os.MkdirAll(d, 0o700) == nil {
		os.Chmod(root, 0o700) // tighten a root a pre-hardening version created loose
	}
	return d
}

// WriteFileAtomic writes data via a temp file and rename, so a concurrent
// reader (another ax process polling a sidecar) never sees a torn or empty
// file mid-write.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ax-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// WriteJSON marshals v and writes it atomically (0600), the shape every
// sidecar store (meta, ask, runs, hosts) uses.
func WriteJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o600)
}
