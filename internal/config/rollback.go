package config

// rollback.go is the recovery half of apply-profile: apply-profile writes a
// config.toml.bak.<numeric-suffix> before every overwrite, and rollback restores the
// most recent such backup over the current config. The restore is atomic (temp
// in the same dir + rename, via atomicWrite) so a torn write never leaves the
// config truncated.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LatestBackup finds the most recent config backup apply-profile wrote next to
// the active config (config.toml.bak.<numeric-suffix>), returning its path and
// numeric suffix. ok is false when no backup exists (so rollback can say so and
// change nothing). "Most recent" is the largest numeric suffix, not lexical
// order, so suffixes of different widths still compare correctly.
func LatestBackup() (path string, suffix int64, ok bool) {
	dir := filepath.Dir(Path())
	prefix := filepath.Base(Path()) + ".bak."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, false
	}
	best := int64(-1)
	var bestName string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		ts, err := strconv.ParseInt(strings.TrimPrefix(name, prefix), 10, 64)
		if err != nil {
			continue // not a numeric-suffixed backup; ignore
		}
		if ts > best {
			best, bestName = ts, name
		}
	}
	if best < 0 {
		return "", 0, false
	}
	return filepath.Join(dir, bestName), best, true
}

// RestoreBackup atomically writes the contents of backupPath over the active
// config at Path() (temp in the same dir + rename). It does not itself create a
// pre-restore backup: the backup being restored is already a snapshot, and
// apply-profile is the only path that snapshots. Returns the read/write error.
func RestoreBackup(backupPath string) error {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return atomicWrite(Path(), data)
}
