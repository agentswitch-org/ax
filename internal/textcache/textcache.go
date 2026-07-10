// Package textcache keeps a plain-text sidecar of each transcript's
// conversation text, so content search greps clean prose instead of raw JSONL
// (no token counts, uuids, or timestamps to match by accident). Sidecars are
// rebuilt only when the transcript is newer than the cache.
package textcache

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/session"
)

func dir() string {
	return axdir.StatePath("text")
}

// pathFor maps a transcript to its sidecar path (hashed to a flat filename).
func pathFor(transcript string) string {
	sum := sha1.Sum([]byte(transcript))
	return filepath.Join(dir(), hex.EncodeToString(sum[:])+".txt")
}

// Ensure returns the sidecar path for a transcript, rebuilding it if missing or
// stale. On any failure it returns "" so callers can fall back to the raw file.
func Ensure(format, transcript string) string {
	cache := pathFor(transcript)
	src, err := os.Stat(transcript)
	if err != nil {
		return ""
	}
	if dst, err := os.Stat(cache); err == nil && !src.ModTime().After(dst.ModTime()) {
		return cache
	}
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		return ""
	}
	if err := os.WriteFile(cache, []byte(session.FullText(format, transcript)), 0o600); err != nil {
		return ""
	}
	return cache
}
