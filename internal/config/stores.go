package config

// A harness's transcripts do not always live in one place. The store list is
// how ax finds every one of them:
//
//   - the primary store (Stores[0]): where the local harness reads and writes,
//     and the only store a resume is served from;
//   - a secondary store the user added (a second glob/db entry);
//   - the harness's own relocation env var (CLAUDE_CONFIG_DIR, CODEX_HOME,
//     PI_CODING_AGENT_DIR, XDG_DATA_HOME): when set to a non-default dir, that
//     is where the harness actually files sessions, so it becomes the primary;
//   - the other side of a WSL boundary: a `claude` on the WSL PATH resolves to
//     the Windows install through interop and files sessions in the Windows
//     home, so ax in WSL must also index the Windows stores (and vice versa) or
//     hand-started sessions are invisible.
//
// ResolveStores fills Harness.Stores from all of these at Load time. Callers
// read EffectiveStores (which falls back to Glob/DB for a hand-built Config in a
// test) and PrimaryStore.

import (
	"os"
	"path/filepath"
	"strings"
)

// Store is one on-disk location ax indexes for a harness. Exactly one of Glob
// (a file-transcript store) or DB (a sqlite store) is set. Paths are already
// home-expanded.
type Store struct {
	Glob    string // transcript glob, or "" for a db store
	DB      string // sqlite path, or "" for a file store
	Label   string // session badge for a foreign store: "", "win", or "wsl"
	Source  string // diagnostic tag: "config", "env:CLAUDE_CONFIG_DIR", "wsl-auto"
	Primary bool   // the store the harness resumes from (Stores[0])
	Xlate   string // recorded-cwd translation for a cross-boundary store: "", "win2unix"
	Root    string // mount root for Xlate (e.g. "/mnt/"); empty otherwise
}

// storeSpec describes where a built-in harness format keeps its sessions, so
// ResolveStores can derive the env-relocated and cross-boundary stores without
// re-deriving them from the glob string.
type storeSpec struct {
	format  string
	envVar  string   // dir-relocating env var; "" if the format has none
	homeSub []string // HOME -> default config root (e.g. {".claude"}, {".pi","agent"})
	fileSub []string // config root -> transcript glob (nil for a db store)
	dbSub   []string // config root -> sqlite db (nil for a file store)
}

var storeSpecs = []storeSpec{
	{format: "claude", envVar: "CLAUDE_CONFIG_DIR", homeSub: []string{".claude"}, fileSub: []string{"projects", "*", "*.jsonl"}},
	{format: "codex", envVar: "CODEX_HOME", homeSub: []string{".codex"}, fileSub: []string{"sessions", "*", "*", "*", "rollout-*.jsonl"}},
	{format: "pi", envVar: "PI_CODING_AGENT_DIR", homeSub: []string{".pi", "agent"}, fileSub: []string{"sessions", "*", "*.jsonl"}},
	// opencode keeps a single sqlite db under the XDG data dir. XDG_DATA_HOME
	// relocates it; the default root is ~/.local/share.
	{format: "opencode", envVar: "XDG_DATA_HOME", homeSub: []string{".local", "share"}, dbSub: []string{"opencode", "opencode.db"}},
}

func specFor(format string) (storeSpec, bool) {
	for _, s := range storeSpecs {
		if s.format == format {
			return s, true
		}
	}
	return storeSpec{}, false
}

// underRoot builds the store glob/db for this format under a config root dir.
func (s storeSpec) underRoot(root string) (glob, db string) {
	if len(s.dbSub) > 0 {
		return "", filepath.Join(append([]string{root}, s.dbSub...)...)
	}
	return filepath.Join(append([]string{root}, s.fileSub...)...), ""
}

// ResolveStores fills each harness's Stores from its glob/db, the format's env
// override, and (when auto discovery is on) the cross-WSL-boundary stores.
// userStores names harnesses whose glob/db the user set explicitly; the env
// override is not applied there, because the user already named the store.
func ResolveStores(cfg *Config, userStores map[string]bool) {
	for i := range cfg.Harnesses {
		h := &cfg.Harnesses[i]
		auto := cfg.AutoStoresOn() && !userStores[h.Name]
		h.Stores = buildStores(*h, auto)
	}
}

func buildStores(h Harness, auto bool) []Store {
	seen := map[string]bool{}
	var out []Store
	push := func(st Store) {
		st.Glob, st.DB = ExpandHome(st.Glob), ExpandHome(st.DB)
		if st.Glob == "" && st.DB == "" {
			return
		}
		key := st.Glob + "\x00" + st.DB
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, st)
	}

	spec, known := specFor(h.Format)

	// The env-relocated store is pushed first, so it becomes the primary when it
	// differs from the built-in default (the harness files sessions there now).
	// When it resolves to the same dir as the config glob it dedupes away and the
	// config store keeps the primary slot.
	if auto && known && spec.envVar != "" {
		if dir := strings.TrimSpace(os.Getenv(spec.envVar)); dir != "" {
			g, db := spec.underRoot(ExpandHome(dir))
			push(Store{Glob: g, DB: db, Source: "env:" + spec.envVar})
		}
	}
	for _, g := range h.Glob {
		push(Store{Glob: g, Source: "config"})
	}
	for _, db := range h.DB {
		push(Store{DB: db, Source: "config"})
	}
	if auto && known {
		for _, st := range foreignStores(spec) {
			push(st)
		}
	}
	if len(out) > 0 {
		out[0].Primary = true
	}
	return out
}

// foreignStores returns the cross-WSL-boundary stores for a format: from inside
// WSL, the Windows homes' stores; from native Windows, the running distros'
// stores. Empty on every machine that is neither.
func foreignStores(spec storeSpec) []Store {
	var out []Store
	if InWSL() {
		root := wslMountRoot()
		for _, home := range windowsHomesFromWSL() {
			cfgRoot := filepath.Join(append([]string{home}, spec.homeSub...)...)
			g, db := spec.underRoot(cfgRoot)
			out = append(out, Store{Glob: g, DB: db, Label: "win", Source: "wsl-auto", Xlate: "win2unix", Root: root})
		}
		return out
	}
	for _, home := range wslHomesFromWindows() {
		cfgRoot := filepath.Join(append([]string{home}, spec.homeSub...)...)
		g, db := spec.underRoot(cfgRoot)
		// A UNC path into the running distro; the recorded cwd is already a POSIX
		// path Windows cannot cd to, so it is left untranslated (visible/searchable,
		// resume best-effort).
		out = append(out, Store{Glob: g, DB: db, Label: "wsl", Source: "wsl-auto"})
	}
	return out
}

// EffectiveStores returns the resolved stores, falling back to synthesizing them
// from Glob/DB for a Config built by hand (a test) that never went through Load.
func (h Harness) EffectiveStores() []Store {
	if len(h.Stores) > 0 {
		return h.Stores
	}
	var out []Store
	for _, g := range h.Glob {
		out = append(out, Store{Glob: ExpandHome(g), Source: "config"})
	}
	for _, db := range h.DB {
		out = append(out, Store{DB: ExpandHome(db), Source: "config"})
	}
	if len(out) > 0 {
		out[0].Primary = true
	}
	return out
}

// PrimaryStore is the store the harness reads/writes and resumes from.
func (h Harness) PrimaryStore() Store {
	if st := h.EffectiveStores(); len(st) > 0 {
		return st[0]
	}
	return Store{}
}

// MountRoot is the WSL automount root when this process runs in WSL (e.g.
// "/mnt/"), empty otherwise. Callers use it to spot a harness binary that
// resolves across the interop boundary.
func MountRoot() string {
	if InWSL() {
		return wslMountRoot()
	}
	return ""
}

// TranslateCwd maps a transcript's recorded working directory into a path usable
// on this machine for a cross-boundary store (a Windows cwd seen from WSL). It is
// the identity for a same-machine store.
func TranslateCwd(st Store, dir string) string {
	if st.Xlate == "win2unix" {
		return winToUnixWSL(dir, st.Root)
	}
	return dir
}
