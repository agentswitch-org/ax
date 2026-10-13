package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/agentswitch-org/ax/internal/axdir"
)

// Transcripts can total hundreds of MB, so parsing every one on each launch is
// the dominant startup cost. The index cache stores each parsed Session keyed by
// file path, format, mtime, and size; unchanged files need no reparse.

// cacheVersion invalidates the whole cache when the parser changes how a Session
// is derived (a bump makes every transcript reparse once). Bump it whenever the
// meaning of a cached field changes, e.g. how Dir is chosen.
const cacheVersion = 8

type cacheEntry struct {
	MTime  int64   `json:"m"`
	Size   int64   `json:"z"`
	Format string  `json:"f"`
	IDRe   string  `json:"r"`
	Sess   Session `json:"s"`
}

type indexCache struct {
	Version int                   `json:"v"`
	Entries map[string]cacheEntry `json:"e"`
}

func indexCachePath() string {
	return axdir.StatePath("index.json")
}

// loaded retains the last parsed cache in-process, keyed by the cache file's
// mtime. A long-lived caller (the picker refresh, fence pollers) calls Index
// every few seconds; without this it would re-read and re-unmarshal the whole
// index.json (every Session struct for every session) on each tick. Gating on
// mtime keeps it correct across processes: saveIndexCache rewrites the file
// (bumping its mtime), and any other ax process that writes it is picked up on
// the next stat. The map is never mutated after construction, so returning the
// shared reference is safe.
var loaded struct {
	sync.Mutex
	path    string
	mtime   int64
	size    int64
	entries map[string]cacheEntry
}

func loadIndexCache() map[string]cacheEntry {
	p := indexCachePath()
	loaded.Lock()
	defer loaded.Unlock()
	if fi, err := os.Stat(p); err == nil {
		mt := fi.ModTime().UnixNano()
		if loaded.entries != nil && loaded.path == p && loaded.mtime == mt && loaded.size == fi.Size() {
			return loaded.entries
		}
		if data, err := os.ReadFile(p); err == nil {
			var c indexCache
			if json.Unmarshal(data, &c) == nil && c.Version == cacheVersion && c.Entries != nil {
				loaded.entries, loaded.mtime = c.Entries, mt
				loaded.path, loaded.size = p, fi.Size()
				return c.Entries
			}
		}
	}
	loaded.entries = nil // no usable file: don't serve a stale in-process copy
	return map[string]cacheEntry{}
}

func saveIndexCache(m map[string]cacheEntry) {
	p := indexCachePath()
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	// Tighten the state dir to 0700 even if an older ax created it 0755, so a
	// readable home on a shared host can't expose what agents ran and where. This
	// runs on nearly every invocation (Index caches here), so it self-heals.
	os.Chmod(filepath.Dir(p), 0o700)
	if data, err := json.Marshal(indexCache{Version: cacheVersion, Entries: m}); err == nil {
		if axdir.WriteFileAtomic(p, data, 0o600) != nil {
			return
		}
		// Another process can replace the file immediately after our rename.
		// Reload once instead of associating our map with its file timestamp.
		loaded.Lock()
		loaded.entries = nil
		loaded.Unlock()
	}
}
