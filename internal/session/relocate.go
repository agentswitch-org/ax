package session

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agentswitch-org/ax/internal/config"
)

// Claude Code files each session under ~/.claude/projects/<mangled-cwd>/ and
// `claude --resume` only searches the folder derived from the current cwd. So
// when a project directory moves, relinking the session (remap) is not enough:
// the transcript must move to the new mangled folder or resume dies with "No
// conversation found with session ID". Harnesses that resolve a session id
// globally (pi, codex, opencode) resume fine from anywhere and need nothing.

// claudeMangle reproduces Claude Code's project-folder name for a cwd: every
// non-alphanumeric byte becomes "-". Verified against the claude binary
// (path.replace(/[^a-zA-Z0-9]/g,"-")). Past 200 chars claude appends a hash we
// can't reproduce, so callers must treat long paths as non-relocatable.
var claudeMangleRe = regexp.MustCompile(`[^a-zA-Z0-9]`)

const claudeMangleMax = 200

func claudeMangle(dir string) (string, bool) {
	m := claudeMangleRe.ReplaceAllString(dir, "-")
	return m, len(m) <= claudeMangleMax
}

// globRoot is the fixed directory prefix of a transcript glob, e.g.
// "~/.claude/projects/*/*.jsonl" -> "/Users/x/.claude/projects".
func globRoot(glob string) string {
	g := config.ExpandHome(glob)
	i := strings.IndexByte(g, '*')
	if i < 0 {
		return ""
	}
	return filepath.Dir(g[:i])
}

// Relocate moves a session's transcript (and its sidecar directory) to where
// its harness will look for it when resumed in s.Dir. Called before building a
// resume command, it self-heals any session stranded by a directory move,
// including moves relinked before this fix existed. A no-op for harnesses
// without a cwd-keyed store, for sessions already in place, and on any doubt
// (missing file, unmappable path): the caller resumes either way, this only
// improves the odds.
func Relocate(h config.Harness, s Session) error {
	if h.Format != "claude" || s.File == "" || s.Dir == "" {
		return nil
	}
	root := globRoot(h.Glob)
	if root == "" {
		return nil
	}
	mangled, ok := claudeMangle(config.ExpandHome(s.Dir))
	if !ok {
		return fmt.Errorf("dir %s mangles past %d chars (claude hashes the tail); cannot relocate", s.Dir, claudeMangleMax)
	}
	want := filepath.Join(root, mangled)
	if filepath.Dir(s.File) == want {
		return nil
	}
	if _, err := os.Stat(s.File); err != nil {
		return nil // already gone (moved by hand, or a stale index entry)
	}
	if err := os.MkdirAll(want, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(want, filepath.Base(s.File))
	if _, err := os.Stat(dst); err == nil {
		return nil // a transcript with this id is already there; claude will use it
	}
	if err := os.Rename(s.File, dst); err != nil {
		return err
	}
	// The per-session sidecar directory (tool-results, subagents) rides along.
	if side := strings.TrimSuffix(s.File, ".jsonl"); side != s.File {
		if st, err := os.Stat(side); err == nil && st.IsDir() {
			os.Rename(side, filepath.Join(want, filepath.Base(side)))
		}
	}
	return nil
}
