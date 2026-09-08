package propel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Fingerprint measures workspace changes, not worker turnover or conversation.
// A watch path selects the artifact that defines progress (also outside Git).
// Otherwise hash HEAD, Git status, and changed/untracked file contents, so edits
// to an already-dirty file count and touching an unchanged file does not.
func Fingerprint(dir, watch string) string {
	h := sha256.New()
	remaining := int64(32 << 20) // bound content reads per observation
	if watch != "" {
		if !filepath.IsAbs(watch) {
			watch = filepath.Join(dir, watch)
		}
		hashFile(h, watch, &remaining)
	} else {
		root, err := gitOutput(dir, "rev-parse", "--show-toplevel")
		if err == nil {
			rootDir := strings.TrimSpace(string(root))
			head, _ := gitOutput(rootDir, "rev-parse", "HEAD")
			h.Write(head)
			status, err := gitOutput(rootDir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
			if err == nil {
				h.Write(status)
				records := strings.Split(string(status), "\x00")
				for i := 0; i < len(records); i++ {
					r := records[i]
					if len(r) < 4 {
						continue
					}
					hashFile(h, filepath.Join(rootDir, r[3:]), &remaining)
					if strings.ContainsAny(r[:2], "RC") {
						i++ // -z rename/copy has a second record for the old path
					}
				}
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func gitOutput(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	c.WaitDelay = time.Second
	return c.Output()
}

func hashFile(h hash.Hash, path string, remaining *int64) {
	fmt.Fprintf(h, "\x00%s\x00", path)
	info, err := os.Lstat(path)
	if err != nil {
		fmt.Fprint(h, "missing")
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(path)
		fmt.Fprint(h, "symlink:", target)
		return
	}
	if !info.Mode().IsRegular() {
		fmt.Fprint(h, info.Mode())
		return
	}
	fmt.Fprintf(h, "%d:%d:", info.Mode(), info.Size())
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprint(h, "unreadable")
		return
	}
	defer f.Close()
	n, _ := io.Copy(h, io.LimitReader(f, *remaining))
	*remaining -= n
	if n < info.Size() {
		// Very large artifacts use metadata for the unread tail. The absolute
		// continuation cap still applies even if metadata changes each turn.
		fmt.Fprint(h, info.ModTime().UnixNano())
	}
}
