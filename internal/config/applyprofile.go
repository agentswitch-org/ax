package config

// applyprofile.go writes an incoming Profile into the LOCAL on-disk config,
// preserving every local field. It deliberately does NOT round-trip the typed
// Config through the TOML encoder: local fields such as Notify carry a custom
// UnmarshalTOML and no encode tags, so re-encoding the struct would rewrite
// `notify = "bell"` into a shape the loader misreads. Instead it decodes the
// raw file into a generic map, overwrites only the profile keys (harnesses
// merged by name, UI keys replaced wholesale), and re-encodes the map, so every
// untyped local value round-trips verbatim.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// uiProfileKeys are the top-level TOML keys the profile owns and replaces
// wholesale on apply (an absent key in the incoming profile clears the target's).
var uiProfileKeys = []string{
	"columns", "column", "keys",
	"detach_prefix", "detach_key", "menu_key",
	"mux_prefix", "mux_group", "default_harness",
}

// ApplyProfileToFile merges p into the raw config at Path(), preserving every
// local field, and writes the result atomically (temp in the SAME dir + rename)
// after backing up the existing file to config.toml.bak.<suffix>. now is the base
// numeric suffix (passed in so the write is deterministic and testable); if that
// name exists, the suffix is incremented until an unused path is created. It
// returns the backup path written (empty when there was no prior file). Comments
// in the original file are not preserved (a programmatic rewrite cannot keep
// them); the numbered backup is the recovery path.
func ApplyProfileToFile(p Profile, now int64) (backup string, err error) {
	path := Path()
	raw := map[string]any{}
	var existing []byte
	if data, rerr := os.ReadFile(path); rerr == nil {
		existing = data
		if err := toml.Unmarshal(data, &raw); err != nil {
			return "", fmt.Errorf("parse existing config %s: %w", path, err)
		}
	} else if !os.IsNotExist(rerr) {
		return "", rerr
	}

	if err := spliceProfile(raw, p); err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(raw); err != nil {
		return "", fmt.Errorf("encode merged config: %w", err)
	}

	if existing != nil {
		backup, err = writeUniqueBackup(path, existing, now)
		if err != nil {
			return "", fmt.Errorf("write backup %s: %w", backup, err)
		}
	}
	if err := atomicWrite(path, buf.Bytes()); err != nil {
		return backup, err
	}
	return backup, nil
}

// spliceProfile overwrites the profile keys of the raw config map with p,
// merging harnesses by name (preserving each target harness's local glob/db) and
// replacing the UI keys wholesale.
func spliceProfile(raw map[string]any, p Profile) error {
	data, err := EncodeProfile(p)
	if err != nil {
		return err
	}
	var pm map[string]any
	if err := toml.Unmarshal(data, &pm); err != nil {
		return err
	}

	// UI keys: wholesale replace (set when present in the profile, clear when not).
	for _, k := range uiProfileKeys {
		if v, ok := pm[k]; ok {
			raw[k] = v
		} else {
			delete(raw, k)
		}
	}

	// Harnesses: merge by name, keeping each target table's local glob/db. If the
	// target has duplicate same-name tables, update all of them so the later table
	// that Load would otherwise let win cannot retain stale profile values.
	rawHarnesses := toMapSlice(raw["harness"])
	for _, hp := range p.Harnesses {
		ph := harnessProfileMap(hp)
		name, _ := ph["name"].(string)
		matched := false
		for i, rh := range rawHarnesses {
			if n, _ := rh["name"].(string); n == name {
				mergeHarnessProfileMap(rh, ph)
				rawHarnesses[i] = rh
				matched = true
			}
		}
		if !matched {
			rawHarnesses = append(rawHarnesses, ph)
		}
	}
	if len(rawHarnesses) > 0 {
		raw["harness"] = rawHarnesses
	}
	return nil
}

func harnessProfileMap(h HarnessProfile) map[string]any {
	m := map[string]any{"name": h.Name}
	for _, f := range []struct {
		key string
		val string
	}{
		{"format", h.Format},
		{"id_regex", h.IDRe},
		{"resume", h.Resume},
		{"resume_input", h.ResumeInput},
		{"resume_input_headless", h.ResumeInputHeadless},
		{"launch", h.Launch},
		{"launch_headless", h.LaunchHeadless},
		{"args", h.Args},
		{"waiting_re", h.WaitingRe},
		{"skip_permissions", h.SkipPermissions},
	} {
		if harnessProfileFieldPresent(h, f.key) {
			m[f.key] = f.val
		}
	}
	return m
}

func mergeHarnessProfileMap(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}

// toMapSlice coerces a decoded TOML array-of-tables value into []map[string]any.
func toMapSlice(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	}
	return nil
}

// atomicWrite writes data to path via a temp file in the SAME directory followed
// by a rename, so a torn or disk-full write leaves the old config intact. The
// temp is fsync'd before the rename.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func writeUniqueBackup(path string, data []byte, base int64) (string, error) {
	for suffix := base; ; suffix++ {
		backup := fmt.Sprintf("%s.bak.%d", path, suffix)
		f, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return backup, err
		}
		if n, err := f.Write(data); err != nil {
			f.Close()
			os.Remove(backup)
			return backup, err
		} else if n != len(data) {
			f.Close()
			os.Remove(backup)
			return backup, io.ErrShortWrite
		}
		if err := f.Close(); err != nil {
			os.Remove(backup)
			return backup, err
		}
		return backup, nil
	}
}
