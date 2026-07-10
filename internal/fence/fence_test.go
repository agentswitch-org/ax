package fence

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		cmd   string
		allow bool
	}{
		// ax itself and its own verbs (a fenced session's whole loop).
		{`ax list --json --run "$AX_RUN"`, true},
		{`"$AX" read abc --since 3`, true},
		{`$AX claude "do the thing" --dir /tmp/x`, true},
		{`ax tag "$AX_SESSION_ID" --outcome success`, true},
		{`ax check`, true},
		// read-only coreutils.
		{`cat file.txt`, true},
		{`grep -n foo bar.go`, true},
		{`ls -la | grep foo | wc -l`, true},
		{`rg pattern .`, true},
		{`find . -name '*.go'`, true},
		{`find . -type f -print`, true},
		{`awk '{print $1}' file`, true},
		{`awk -F: '$3 > 1000 {print $1}' /etc/passwd`, true},
		{`sort -n file`, true},
		{`sort -u -k2 file`, true},
		{`jq .foo file.json`, true},
		{`sed 's/a/b/' file`, true},
		{`sed -n '1,5p' file`, true},
		// git read subcommands.
		{`git status`, true},
		{`git log --oneline -20`, true},
		{`git diff HEAD~1`, true},
		{`git -C /repo show HEAD`, true},
		{`git worktree list`, true},
		// pipelines/joins where every segment is read-only.
		{`git status && ax list`, true},
		{`cat f | jq . | head`, true},

		// quote-aware splitting: a multi-line double-quoted argument to ax that
		// contains words naming write operations stays ONE allowed segment; its
		// inner operators are literal text, not separators or redirects.
		{"$AX claude \"do the work && git commit -m x\n then rm -rf tmp > out\"", true},
		{`ax claude "review this: echo hi > file; git push origin main"`, true},
		// fd duplications and allowed redirect targets are not file writes.
		{`echo hi 2>&1`, true},
		{`ls -la >&2`, true},
		{`grep foo bar.go 2>/dev/null`, true},
		{`cat file.txt > /dev/null`, true},
		// a double-quoted `>` is literal text, not a redirect.
		{`echo "a > b"`, true},
		{`grep -n "x > y" file`, true},
		// a backslash-escaped quote is a literal char and must not toggle quote
		// state: a `>` inside a `\"..."`-escaped span stays quoted text, so a
		// legitimate launch carrying a nested quoted redirect-looking string is
		// not false-flagged as a real redirect.
		{`ax claude "run: printf \"hello > world\" now"`, true},
		{`ax claude "echo \"x >> y\" done"`, true},
		// escaped single-quote handled the same way.
		{`ax claude "it \'a > b\' ok"`, true},
		// a backslash-escaped `>` outside quotes is a literal char, not a redirect.
		{`echo a \> b`, true},
		// newly allowed read-only commands.
		{`env FOO=bar BAZ=qux`, true},
		{`env -i -u PATH FOO=bar`, true},
		{`printf '%s\n' hello`, true},
		{`cd /some/path`, true},
		// newly allowed git reads.
		{`git remote -v`, true},
		{`git remote show origin`, true},
		{`git remote get-url origin`, true},
		{`git config --get user.name`, true},
		{`git config --list`, true},
		{`git config -l`, true},
		{`git tag -l`, true},
		{`git tag`, true},
		{`git cat-file -p HEAD`, true},
		{`git rev-list --count HEAD`, true},
		{`git for-each-ref`, true},
		{`git describe --tags`, true},
		{`git blame README.md`, true},
		// redirect tokens must not leak into subcommand parsers.
		{`git remote -v 2>&1`, true},
		{`git log 2>/dev/null`, true},
		{`git config --get x 2>&1`, true},
		{`git grep pattern`, true},
		{`git status > /dev/null`, true},

		// redirects to a real file (including append) are still writes.
		{`echo x > realfile`, false},
		{`echo x >> realfile`, false},
		{`printf hi > realfile`, false},
		// after an escaped-quote span closes, a real redirect outside quotes is
		// still denied: the scanner must recover correct quote state, not stay
		// stuck "inside" a quote and swallow the write.
		{`printf "a \"b\" c" > realfile`, false},
		{`echo \"quoted\" > realfile`, false},
		// an escaped `\>` is a literal, not a redirect, so it cannot form a `>&`
		// fd-duplication: a following `&` must still split the command, and the
		// write command after it must be classified on its own (not glued onto
		// the allowlisted echo/ls/printf segment).
		{`echo x \>& rm -rf foo`, false},
		{`ls \>& touch pwned`, false},
		{`printf hi \>& rm -rf x`, false},
		// env execs the command that follows: a bare command word is denied.
		{`env FOO=bar rm -rf x`, false},
		{`env -i sh -c 'rm x'`, false},
		// git remote/config/tag writes stay denied.
		{`git remote add origin url`, false},
		{`git remote set-url origin url`, false},
		{`git config user.name bob`, false},
		{`git config --add user.name bob`, false},
		{`git tag v1`, false},
		{`git tag -d v1`, false},
		{`git fetch`, false},
		{`git pull`, false},
		{`git clone https://x`, false},
		{`git stash`, false},

		// writes and escapes: denied.
		{`rm -rf /tmp/x`, false},
		{`echo hi > out.txt`, false},
		{`cat a >> b`, false},
		{`sed -i 's/a/b/' file`, false},
		{`sed -i.bak 's/a/b/' file`, false},
		{`tee out.txt`, false},
		// find/awk/sort write and exec forms are denied (they passed as innocent
		// read commands before this fence hardening).
		{`find . -name '*.tmp' -delete`, false},
		{`find . -exec sh -c 'rm {}' \;`, false},
		{`find . -execdir rm {} +`, false},
		{`find . -fprintf out.txt '%p\n'`, false},
		{`find . -fprint out.txt`, false},
		{`awk 'BEGIN{system("rm -rf x")}'`, false},
		{`awk '{print > "out.txt"}' file`, false},
		{`awk '{print $0 >> "log"}' file`, false},
		{`awk '{print | "sh"}' file`, false},
		{`sort -o out.txt file`, false},
		{`sort file -o out.txt`, false},
		{`sort --output=out.txt file`, false},
		{`python script.py`, false},
		{`node build.js`, false},
		{`sh -c 'rm x'`, false},
		{`bash deploy.sh`, false},
		{`eval "$x"`, false},
		{`xargs rm`, false},
		{`npm install`, false},
		{`pip install requests`, false},
		{`git commit -m 'x'`, false},
		{`git push origin main`, false},
		{`git add -A`, false},
		{`git checkout -b feature`, false},
		{`git reset --hard`, false},
		{`git worktree add /tmp/x -b run/x`, false},
		// mixed pipeline: one write poisons the whole line.
		{`cat f && rm g`, false},
		{`git status; python x.py`, false},
	}
	for _, c := range cases {
		got := Classify(c.cmd)
		if got.Allow != c.allow {
			t.Errorf("Classify(%q).Allow = %v, want %v (reason=%q)", c.cmd, got.Allow, c.allow, got.Reason)
		}
	}
}

// TestClassifyDenylistWordsAsArgs pins the core guarantee: a read-only command
// is allowed even when its arguments contain words that name a write operation
// (role, which, command, codex, commit, coordination, rm, add, push). The fence
// must match the executable and the shell operators, not substrings of the line.
func TestClassifyDenylistWordsAsArgs(t *testing.T) {
	allowed := []string{
		`grep -rn role .`,
		`grep coordination internal/`,
		`grep -n commit README.md`,
		`grep -e codex -e push logs.txt`,
		`ls -la role which command codex`,
		`cat commit.txt role.md coordination.go`,
		`git log --oneline --grep=commit`,
		`git log --format='%H %s' -- add.go push.go`,
		`git diff --stat worktree.go`,
		`rg 'git commit' .`,
		`which codex`,
		`command -v codex`,
		`command -V rm`,
		`ax read "$AX_RUN" --since 3`,
	}
	for _, c := range allowed {
		if d := Classify(c); !d.Allow {
			t.Errorf("Classify(%q).Allow = false, want true (reason=%q)", c, d.Reason)
		}
	}
}

// TestClassifyWritesStillDenied pins the security guarantee: real writes stay
// blocked regardless of the denylist-word allowances above.
func TestClassifyWritesStillDenied(t *testing.T) {
	denied := []string{
		`echo role > out.txt`,
		`grep role file >> log`,
		`git commit -m 'role change'`,
		`git add coordination.go`,
		`git push origin main`,
		`git worktree add /tmp/x -b run/x`,
		`rm -rf coordination`,
		`mv role.go role.bak`,
		`sed -i 's/which/that/' file`,
		`tee role.txt`,
		`command rm -rf x`,
		`grep role . && rm x`,
	}
	for _, c := range denied {
		if d := Classify(c); d.Allow {
			t.Errorf("Classify(%q).Allow = true, want false", c)
		}
	}
}

func TestClassifyWithExtraAllow(t *testing.T) {
	if Classify("make build").Allow {
		t.Fatal("make should be denied by default")
	}
	if !ClassifyWith("make build", []string{"make"}).Allow {
		t.Fatal("make should be allowed via extra allow list")
	}
	// An extra-allowed exec still cannot smuggle a redirect.
	if ClassifyWith("make build > out", []string{"make"}).Allow {
		t.Fatal("a redirect must be denied even for an extra-allowed exec")
	}
}

func TestAllowedTool(t *testing.T) {
	for _, ok := range []string{"Read", "Glob", "Grep", "LS", "NotebookRead", "WebFetch", "WebSearch", "TodoWrite"} {
		if !AllowedTool(ok) {
			t.Errorf("AllowedTool(%q) = false, want true", ok)
		}
	}
	for _, no := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit", "Task", "Bash", "mcp__foo__bar", ""} {
		if AllowedTool(no) {
			t.Errorf("AllowedTool(%q) = true, want false", no)
		}
	}
}

// TestWriteAllowed pins the write-scope glob matcher: a target is allowed iff it
// matches at least one glob, judged on the safe (cleaned + symlink-resolved)
// absolute path, so `../` traversal and symlink escapes cannot pass. The root is a
// real temp dir so ancestor resolution is exercised end to end.
func TestWriteAllowed(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".state", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	j := filepath.Join

	cases := []struct {
		name   string
		globs  []string
		target string
		want   bool
	}{
		// ** matches any number of segments including zero.
		{"doublestar zero segs", []string{j(root, "**", "*.md")}, j(root, "a.md"), true},
		{"doublestar many segs", []string{j(root, "**", "*.md")}, j(root, "b", "c", "d.md"), true},
		{"doublestar wrong ext", []string{j(root, "**", "*.md")}, j(root, "b", "c.go"), false},
		// * matches within a single segment only, never crossing a separator.
		{"star single seg match", []string{j(root, "out", "*.md")}, j(root, "out", "notes.md"), true},
		{"star no cross sep", []string{j(root, "out", "*.md")}, j(root, "out", "sub", "notes.md"), false},
		// ? matches a single non-separator char.
		{"question match", []string{j(root, "v?.md")}, j(root, "v1.md"), true},
		{"question no match multi", []string{j(root, "v?.md")}, j(root, "v12.md"), false},
		// A trailing *.md matches the final segment's name.
		{"state tree top", []string{j(root, ".state", "**", "*.md")}, j(root, ".state", "backlog.md"), true},
		{"state tree deep", []string{j(root, ".state", "**", "*.md")}, j(root, ".state", "sub", "state.md"), true},
		{"state tree wrong ext", []string{j(root, ".state", "**", "*.md")}, j(root, ".state", "sub", "x.sh"), false},
		// Prefix collision: /foo/bar/** must NOT match /foo/barbaz/x.
		{"prefix collision", []string{j(root, "bar", "**")}, j(root, "barbaz", "x"), false},
		{"prefix exact ok", []string{j(root, "bar", "**")}, j(root, "bar", "x"), true},
		// A ../ target is cleaned before matching: it collapses within the root
		// (still not under the pattern's subdir) and cannot escape it.
		{"dotdot within root", []string{j(root, "in", "**", "*.md")}, j(root, "in", "..", "out.md"), false},
		{"dotdot escapes root", []string{j(root, "**", "*.md")}, j(root, "..", "evil.md"), false},
		// Multiple globs: a match on any one allows.
		{"second glob matches", []string{j(root, "a", "*.md"), j(root, "b", "*.md")}, j(root, "b", "x.md"), true},
		{"no glob matches", []string{j(root, "a", "*.md"), j(root, "b", "*.md")}, j(root, "c", "x.md"), false},
		// Anchored: a target above the pattern's root does not match.
		{"target above pattern root", []string{j(root, "deep", "**")}, j(root, "shallow.md"), false},
		// Empty globs (the --no-write case) and empty target always deny.
		{"empty globs", nil, j(root, "a.md"), false},
		{"empty target", []string{j(root, "**")}, "", false},
	}
	for _, c := range cases {
		if got := WriteAllowed(c.globs, root, c.target); got != c.want {
			t.Errorf("%s: WriteAllowed(%v, %q) = %v, want %v", c.name, c.globs, c.target, got, c.want)
		}
	}

	// A relative target resolves against cwd; a relative ../ escape resolves
	// outside the root and is denied; a relative target with no cwd cannot resolve.
	if !WriteAllowed([]string{j(root, "**", "*.md")}, root, "notes.md") {
		t.Error("relative target under cwd must match")
	}
	if WriteAllowed([]string{j(root, "**", "*.md")}, root, filepath.Join("..", "escape.md")) {
		t.Error("relative ../ escape must be denied")
	}
	if WriteAllowed([]string{j(root, "**")}, "", "notes.md") {
		t.Error("relative target with empty cwd must be denied")
	}
}

// TestWriteAllowedSymlinkEscape proves a symlink whose real target lands outside
// every glob root cannot smuggle a write past the fence, even when the symlink's
// own lexical path (or name) would match.
func TestWriteAllowedSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	globs := []string{filepath.Join(root, "**", "*.md")}

	// A symlink dir inside root that points outside it: a write "under" it resolves
	// outside root, so it must be denied though the lexical path is under root.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if WriteAllowed(globs, "", filepath.Join(link, "state.md")) {
		t.Error("a write through a symlink escaping the glob root must be denied")
	}

	// A genuine path under root still matches.
	if !WriteAllowed(globs, "", filepath.Join(root, "state.md")) {
		t.Error("a real path under the glob root must match")
	}

	// A symlink named foo.md inside root whose real target is a *.md file outside
	// root must be denied, even though its own name matches *.md.
	if err := os.WriteFile(filepath.Join(outside, "target.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkMd := filepath.Join(root, "foo.md")
	if err := os.Symlink(filepath.Join(outside, "target.md"), linkMd); err != nil {
		t.Skipf("cannot create foo.md symlink: %v", err)
	}
	if WriteAllowed(globs, "", linkMd) {
		t.Error("a symlink named foo.md resolving outside the glob root must be denied")
	}
}

// TestDeniedToolsFor pins the deny-list difference the write scope makes: without
// a write glob (writable=false, the --no-write case) every write tool is denied;
// with one (writable=true) the file-write tools leave the list (the hook gates
// them by path) but the rest stay denied.
func TestDeniedToolsFor(t *testing.T) {
	has := func(xs []string, x string) bool {
		for _, v := range xs {
			if v == x {
				return true
			}
		}
		return false
	}
	denied := deniedToolsFor(false, false)
	for _, tool := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit", "mcp__*"} {
		if !has(denied, tool) {
			t.Errorf("--no-write deny list must contain %q", tool)
		}
	}
	writable := deniedToolsFor(true, false)
	for _, tool := range []string{"Write", "Edit", "MultiEdit"} {
		if has(writable, tool) {
			t.Errorf("writable deny list must NOT contain %q (the hook gates it)", tool)
		}
	}
	for _, tool := range []string{"NotebookEdit", "mcp__*"} {
		if !has(writable, tool) {
			t.Errorf("writable deny list must still contain %q", tool)
		}
	}
}

// TestDeniedToolsForNoSubagents pins the --no-subagents backstop: the sub-agent
// spawn tools join the deny list only when noSubagents is set, independent of the
// write scope.
func TestDeniedToolsForNoSubagents(t *testing.T) {
	has := func(xs []string, x string) bool {
		for _, v := range xs {
			if v == x {
				return true
			}
		}
		return false
	}
	for _, writable := range []bool{false, true} {
		on := deniedToolsFor(writable, true)
		for _, tool := range subagentTools {
			if !has(on, tool) {
				t.Errorf("deniedToolsFor(%v, true) must contain %q", writable, tool)
			}
		}
		off := deniedToolsFor(writable, false)
		for _, tool := range subagentTools {
			if has(off, tool) {
				t.Errorf("deniedToolsFor(%v, false) must NOT contain %q", writable, tool)
			}
		}
	}
}

// TestApplyWriteGlobsDropsWriteTools proves Apply's emitted settings drop the
// file-write tools when a write glob is present, and keep them denied otherwise.
func TestApplyWriteGlobsDropsWriteTools(t *testing.T) {
	_, noWrite, err := Apply("claude", Options{HookCmd: "/abs/ax fence-check"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(noWrite, `"Write"`) {
		t.Fatalf("--no-write fence settings must deny Write: %s", noWrite)
	}
	_, writable, err := Apply("claude", Options{HookCmd: "/abs/ax fence-check", WriteGlobs: []string{"/tmp/scr/**/*.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(writable, `"Write"`) || strings.Contains(writable, `"Edit"`) {
		t.Fatalf("writable fence settings must not deny Write/Edit: %s", writable)
	}
	if !strings.Contains(writable, "NotebookEdit") || !strings.Contains(writable, "mcp__*") {
		t.Fatalf("writable fence settings must still deny NotebookEdit/mcp__*: %s", writable)
	}
}

func TestApplyClaude(t *testing.T) {
	args, js, err := Apply("claude", Options{HookCmd: "/abs/ax fence-check"})
	if err != nil {
		t.Fatalf("Apply(claude) err = %v", err)
	}
	if len(args) != 2 || args[0] != "--settings" {
		t.Fatalf("Apply(claude) args = %v, want [--settings <json>]", args)
	}
	if args[1] != js {
		t.Fatalf("settings arg and returned JSON differ")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatalf("settings JSON is invalid: %v", err)
	}
	if !strings.Contains(js, `"deny"`) || !strings.Contains(js, `"Write"`) || !strings.Contains(js, `"mcp__*"`) {
		t.Fatalf("settings JSON missing deny list: %s", js)
	}
	if !strings.Contains(js, "PreToolUse") || !strings.Contains(js, "/abs/ax fence-check") {
		t.Fatalf("settings JSON missing PreToolUse hook: %s", js)
	}
}

func TestApplyUnsupported(t *testing.T) {
	for _, f := range []string{"codex", "pi", "opencode", "unknown"} {
		if _, _, err := Apply(f, Options{}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Apply(%q) err = %v, want ErrUnsupported", f, err)
		}
	}
}
