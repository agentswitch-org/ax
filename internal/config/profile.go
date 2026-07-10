package config

// This file implements the portable "profile" subset of a Config and the push
// model behind `ax config sync`: the key-holding machine is the source of truth
// and pushes its profile over the existing ssh transport to each target host,
// overwriting that host's profile fields while preserving every LOCAL setting.
//
// The split is a FIXED classification in code, not a file split:
//
//	PROFILE (portable, synced): per-harness TEMPLATE fields merged BY NAME
//	  (Launch, LaunchHeadless, Resume, ResumeInput, ResumeInputHeadless, Format,
//	  Args, WaitingRe, IDRe, SkipPermissions) plus UI/behavior (Columns,
//	  ColumnDefaults, Keys, DetachPrefix, DetachKey, MenuKey, MuxPrefix, MuxGroup,
//	  DefaultHarness).
//	LOCAL (never synced, preserved on the target): Hosts, per-harness Glob + DB,
//	  Shell, BehaviorsDir, RecipesDir, Mux, HoldBackend, Notify, Binds, Fence,
//	  Policy, Metrics, Retention, Offline.
//
// Excluding Shell/Mux/Glob/DB and compose directories sidesteps cross-OS harm
// (a macOS glob/path or a pwsh shell must not land on a Windows box); excluding
// Notify/Binds/Fence/Policy/Retention sidesteps secret, safety-control, and
// local lifecycle propagation.
// This conservative profile is deliberate. The threat model is a trusted
// operator (the ssh-key holder is the admin): this is footgun-avoidance, not
// intruder-defense.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"

	"github.com/BurntSushi/toml"
)

// Profile is the portable subset of a Config. It carries only the fields that
// are safe to overwrite on another machine; every path-, OS-, secret-, or
// safety-shaped field is deliberately absent (see the file comment). It marshals
// to TOML for `ax config export-profile` and is what `ax config apply-profile`
// reads back on the target.
type Profile struct {
	// Harnesses hold only each harness's portable template half, keyed by Name.
	// The target's machine-local Glob/DB are NOT here and are preserved on apply.
	Harnesses []HarnessProfile `toml:"harness,omitempty"`
	// Columns / ColumnDefaults / Keys and the chord/mux-naming/default fields
	// below are pure UI/behavior preference: portable verbatim.
	Columns        []string              `toml:"columns,omitempty"`
	ColumnDefaults []ColumnDefault       `toml:"column,omitempty"`
	Keys           map[string]StringList `toml:"keys,omitempty"`
	DetachPrefix   string                `toml:"detach_prefix,omitempty"`
	DetachKey      string                `toml:"detach_key,omitempty"`
	MenuKey        string                `toml:"menu_key,omitempty"`
	MuxPrefix      string                `toml:"mux_prefix,omitempty"`
	MuxGroup       string                `toml:"mux_group,omitempty"`
	DefaultHarness string                `toml:"default_harness,omitempty"`
}

// HarnessProfile is the portable half of a Harness: its name (the merge key) and
// the command templates / parser selection. Glob and DB are intentionally absent
// because they are machine/OS-local paths.
type HarnessProfile struct {
	Name                string `toml:"name"`
	Format              string `toml:"format"`
	IDRe                string `toml:"id_regex"`
	Resume              string `toml:"resume"`
	ResumeInput         string `toml:"resume_input"`
	ResumeInputHeadless string `toml:"resume_input_headless"`
	Launch              string `toml:"launch"`
	LaunchHeadless      string `toml:"launch_headless"`
	Args                string `toml:"args"`
	WaitingRe           string `toml:"waiting_re"`
	SkipPermissions     string `toml:"skip_permissions"`

	// present is filled by DecodeProfile for backward-compatible partial profiles.
	// A nil map means a full profile, which is what Profile()/EncodeProfile produce.
	present map[string]bool
}

// InSyncMarker is printed by `ax config apply-profile` when the incoming profile
// changes nothing on the target. `ax config sync` matches it in the remote's
// output to report a host as already in sync rather than applied.
const InSyncMarker = "profile already in sync"

// Profile extracts the portable subset of c, dropping every LOCAL field.
func (c Config) Profile() Profile {
	p := Profile{
		Columns:        c.Columns,
		ColumnDefaults: c.ColumnDefaults,
		Keys:           c.Keys,
		DetachPrefix:   c.DetachPrefix,
		DetachKey:      c.DetachKey,
		MenuKey:        c.MenuKey,
		MuxPrefix:      c.MuxPrefix,
		MuxGroup:       c.MuxGroup,
		DefaultHarness: c.DefaultHarness,
	}
	for _, h := range c.Harnesses {
		p.Harnesses = append(p.Harnesses, HarnessProfile{
			Name:                h.Name,
			Format:              h.Format,
			IDRe:                h.IDRe,
			Resume:              h.Resume,
			ResumeInput:         h.ResumeInput,
			ResumeInputHeadless: h.ResumeInputHeadless,
			Launch:              h.Launch,
			LaunchHeadless:      h.LaunchHeadless,
			Args:                h.Args,
			WaitingRe:           h.WaitingRe,
			SkipPermissions:     h.SkipPermissions,
		})
	}
	return p
}

// EncodeProfile renders p as TOML for stdout / the sync pipe.
func EncodeProfile(p Profile) ([]byte, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeProfile parses a profile TOML document (from stdin on the target).
func DecodeProfile(data []byte) (Profile, error) {
	var p Profile
	if err := toml.Unmarshal(data, &p); err != nil {
		return p, err
	}
	var raw struct {
		Harnesses []map[string]any `toml:"harness"`
	}
	if err := toml.Unmarshal(data, &raw); err != nil {
		return p, err
	}
	for i := range p.Harnesses {
		if i >= len(raw.Harnesses) {
			break
		}
		p.Harnesses[i].present = map[string]bool{}
		for k := range raw.Harnesses[i] {
			p.Harnesses[i].present[k] = true
		}
	}
	return p, nil
}

// ProfileHash is the SHA-256 (hex) of p's canonical TOML encoding. Because
// EncodeProfile is deterministic (stable key order, omitempty), two nodes hash
// to the same value iff their portable profiles are byte-identical, so a hash
// match is an exact in-sync signal. Both the node self-report (the wire
// Capability block) and `ax config status`'s local side compute it the same way,
// so the comparison is apples-to-apples. Returns "" if p cannot be encoded.
func ProfileHash(p Profile) string {
	data, err := EncodeProfile(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ProfileFromBytes parses a full config.toml document and extracts its portable
// profile, WITHOUT merging the built-in defaults. It is used to diff two config
// files (the current one against a backup) on the same footing. A parse error is
// returned; unknown/local keys are simply ignored by the Profile projection.
func ProfileFromBytes(data []byte) (Profile, error) {
	var c Config
	if err := toml.Unmarshal(data, &c); err != nil {
		return Profile{}, err
	}
	return c.Profile(), nil
}

// secretRes match a value that must never ride in a profile: an API key, a
// bearer token, a URL/query token, a password, or an api-key-shaped field.
var secretRes = []*regexp.Regexp{
	regexp.MustCompile(`sk-`),
	regexp.MustCompile(`Bearer `),
	regexp.MustCompile(`token=`),
	regexp.MustCompile(`(?i)password`),
	regexp.MustCompile(`(?i)api[_-]?key`),
}

// homeRes match an absolute home path that would leak the operator's username
// (and is meaningless on the target's filesystem).
var homeRes = []*regexp.Regexp{
	regexp.MustCompile(`/Users/`),
	regexp.MustCompile(`/home/`),
	regexp.MustCompile(`(?i)C:/Users/`),
	regexp.MustCompile(`(?i)C:\\Users\\`),
}

// LintProfile refuses a profile whose any value matches a secret pattern or an
// absolute home path, naming the offending field. It runs on the SENDER
// (export-profile / sync) so a profile physically cannot carry a secret or a
// local path off the box, enforcing the invariant the classification already
// aims for. Returns nil for a clean profile.
func LintProfile(p Profile) error {
	check := func(field, val string) error {
		for _, re := range secretRes {
			if re.MatchString(val) {
				return fmt.Errorf("profile field %s looks like it carries a secret (matched %q); profiles must carry no secrets (use env vars locally)", field, re.String())
			}
		}
		for _, re := range homeRes {
			if re.MatchString(val) {
				return fmt.Errorf("profile field %s contains an absolute home path (matched %q); profiles must carry no local paths", field, re.String())
			}
		}
		return nil
	}
	for _, h := range p.Harnesses {
		if err := check("harness.name", h.Name); err != nil {
			return err
		}
		for _, f := range []struct{ name, val string }{
			{"harness[" + h.Name + "].format", h.Format},
			{"harness[" + h.Name + "].id_regex", h.IDRe},
			{"harness[" + h.Name + "].resume", h.Resume},
			{"harness[" + h.Name + "].resume_input", h.ResumeInput},
			{"harness[" + h.Name + "].resume_input_headless", h.ResumeInputHeadless},
			{"harness[" + h.Name + "].launch", h.Launch},
			{"harness[" + h.Name + "].launch_headless", h.LaunchHeadless},
			{"harness[" + h.Name + "].args", h.Args},
			{"harness[" + h.Name + "].waiting_re", h.WaitingRe},
			{"harness[" + h.Name + "].skip_permissions", h.SkipPermissions},
		} {
			if err := check(f.name, f.val); err != nil {
				return err
			}
		}
	}
	for _, f := range []struct{ name, val string }{
		{"detach_prefix", p.DetachPrefix},
		{"detach_key", p.DetachKey},
		{"menu_key", p.MenuKey},
		{"mux_prefix", p.MuxPrefix},
		{"mux_group", p.MuxGroup},
		{"default_harness", p.DefaultHarness},
	} {
		if err := check(f.name, f.val); err != nil {
			return err
		}
	}
	for _, col := range p.Columns {
		if err := check("columns", col); err != nil {
			return err
		}
	}
	for i, col := range p.ColumnDefaults {
		if err := check(fmt.Sprintf("column[%d].key", i), col.Key); err != nil {
			return err
		}
	}
	for action, keys := range p.Keys {
		if err := check("keys."+action, action); err != nil {
			return err
		}
		for _, k := range keys {
			if err := check("keys."+action, k); err != nil {
				return err
			}
		}
	}
	return nil
}

// DiffProfile returns human-readable lines describing what applying inc onto a
// target whose current effective profile is cur would change. An empty slice
// means "no change" (the idempotency / already-in-sync signal). Only fields inc
// would touch are reported: harness template fields present in the incoming
// profile (exported profiles include every one, so an empty string clears), and
// the UI/behavior fields (replaced wholesale, so clearing a value is a change too).
func DiffProfile(cur, inc Profile) []string {
	var out []string
	curH := map[string]HarnessProfile{}
	for _, h := range cur.Harnesses {
		curH[h.Name] = h
	}
	for _, h := range inc.Harnesses {
		c, ok := curH[h.Name]
		if !ok {
			out = append(out, fmt.Sprintf("+ harness[%s] added", h.Name))
			continue
		}
		for _, f := range []struct{ field, a, b string }{
			{"format", c.Format, h.Format},
			{"id_regex", c.IDRe, h.IDRe},
			{"resume", c.Resume, h.Resume},
			{"resume_input", c.ResumeInput, h.ResumeInput},
			{"resume_input_headless", c.ResumeInputHeadless, h.ResumeInputHeadless},
			{"launch", c.Launch, h.Launch},
			{"launch_headless", c.LaunchHeadless, h.LaunchHeadless},
			{"args", c.Args, h.Args},
			{"waiting_re", c.WaitingRe, h.WaitingRe},
			{"skip_permissions", c.SkipPermissions, h.SkipPermissions},
		} {
			if harnessProfileFieldPresent(h, f.field) && f.a != f.b {
				out = append(out, fmt.Sprintf("~ harness[%s].%s: %q -> %q", h.Name, f.field, f.a, f.b))
			}
		}
	}
	for _, f := range []struct{ field, a, b string }{
		{"detach_prefix", cur.DetachPrefix, inc.DetachPrefix},
		{"detach_key", cur.DetachKey, inc.DetachKey},
		{"menu_key", cur.MenuKey, inc.MenuKey},
		{"mux_prefix", cur.MuxPrefix, inc.MuxPrefix},
		{"mux_group", cur.MuxGroup, inc.MuxGroup},
		{"default_harness", cur.DefaultHarness, inc.DefaultHarness},
	} {
		if f.a != f.b {
			out = append(out, fmt.Sprintf("~ %s: %q -> %q", f.field, f.a, f.b))
		}
	}
	if !reflect.DeepEqual(cur.Columns, inc.Columns) {
		out = append(out, fmt.Sprintf("~ columns: %v -> %v", cur.Columns, inc.Columns))
	}
	if !reflect.DeepEqual(cur.ColumnDefaults, inc.ColumnDefaults) {
		out = append(out, "~ column defaults changed")
	}
	if !reflect.DeepEqual(cur.Keys, inc.Keys) {
		out = append(out, "~ key bindings changed")
	}
	return out
}

func harnessProfileFieldPresent(h HarnessProfile, field string) bool {
	return h.present == nil || h.present[field]
}
