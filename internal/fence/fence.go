// Package fence is the read-only capability fence for a read-only session: it
// makes a session physically unable to edit files or run mutating shell, so it
// must delegate writes to sessions it launches. It is a deny-by-default ALLOWLIST, not a
// denylist: only the tools and commands named here get through, everything else
// is refused.
//
// Two enforcement surfaces:
//   - Apply builds the harness-specific launch flags (for claude, a --settings
//     blob that denies the write tools and installs a PreToolUse hook).
//   - Classify / AllowedTool are the allowlist the PreToolUse hook (`ax
//     fence-check`) consults per tool call: AllowedTool for the tool name, and
//     Classify for a Bash command line (deny-by-default over every pipeline
//     segment).
//
// The allowlist is leaky by construction (a determined agent can smuggle a write
// past a shell allowlist). That is acceptable for a compliant fenced session we are
// nudging toward delegation; only OS-level isolation is un-foolable.
package fence

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agentswitch-org/ax/internal/config"
)

// ErrUnsupported means the harness has no mechanism ax can use to enforce a
// read-only fence, so Launch must refuse (fail closed) unless best-effort.
var ErrUnsupported = errors.New("harness cannot enforce a read-only fence")

// allowedTools is the read-only tool allowlist. Bash is deliberately absent: it
// is allowed only when its command passes Classify (handled by the hook), never
// unconditionally.
var allowedTools = map[string]bool{
	"Read":         true,
	"Glob":         true,
	"Grep":         true,
	"LS":           true,
	"NotebookRead": true,
	"WebFetch":     true,
	"WebSearch":    true,
	"TodoWrite":    true,
}

// writeTools are the file-editing tools. They are denied wholesale under a
// --no-write fence, but when the fence carries a write scope (see WriteAllowed)
// the PreToolUse hook allows them for a path matching one of the write globs, so
// they are dropped from the settings deny list in that mode and gated per-call by
// the hook instead.
var writeTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true}

// subagentTools are the sub-agent spawn tools. A --no-subagents fenced session is
// barred from them and must delegate via `ax claude ...` (so the work is tracked,
// attachable, and fenced); an ordinary fenced session keeps them.
var subagentTools = []string{"Task", "TaskCreate", "Agent"}

// IsSubagentTool reports whether name is a sub-agent spawn tool (Task/TaskCreate/
// Agent), the set a --no-subagents session is barred from.
func IsSubagentTool(name string) bool {
	for _, t := range subagentTools {
		if name == t {
			return true
		}
	}
	return false
}

// deniedToolsFor names the mutating tools placed in the harness settings deny
// list, a backstop to the PreToolUse hook (which is the real authority). mcp__*
// is denied wholesale: an MCP tool can do anything, so none are on the allowlist.
// When writable is true (the fence carries at least one write glob), the
// file-write tools are omitted so the hook can allow them for a matching path;
// every other mutating tool stays denied. When writable is false (the --no-write
// case) the file-write tools stay denied. When noSubagents is set, the sub-agent
// spawn tools are added to the backstop.
func deniedToolsFor(writable, noSubagents bool) []string {
	base := []string{"NotebookEdit", "mcp__*"}
	if noSubagents {
		base = append(base, subagentTools...)
	}
	if writable {
		return base
	}
	return append([]string{"Write", "Edit", "MultiEdit"}, base...)
}

// AllowedTool reports whether a tool name is on the read-only allowlist. Bash is
// not (it is gated by Classify), and anything unlisted is denied.
func AllowedTool(name string) bool { return allowedTools[name] }

// IsWriteTool reports whether name is a file-editing tool gated by the write
// scope (Write/Edit/MultiEdit). These are denied outright under --no-write and
// gated by WriteAllowed when the fence carries write globs.
func IsWriteTool(name string) bool { return writeTools[name] }

// WriteAllowed reports whether a write to target is permitted by the write scope:
// it returns true iff target matches at least one glob in globs, judged on the
// SAFE normalized path. It is the write scope's boundary check, and it is
// traversal- and symlink-proof by construction: the target is made absolute
// (relative targets resolve against cwd, the session's workspace), cleaned (which
// collapses any `../`), then symlink-resolved on its longest existing ancestor
// (so a symlinked ancestor cannot mask an escape even before the final file
// exists). A target that resolves outside every pattern's real root therefore
// cannot match. Each pattern is normalized the same way (expand ~, make absolute
// against cwd, clean) but keeps its glob metacharacters (`**`, `*`, `?`), and the
// whole target must match the whole pattern (anchored). Empty globs (the
// --no-write case) or an empty target always returns false.
func WriteAllowed(globs []string, cwd, target string) bool {
	if len(globs) == 0 || target == "" {
		return false
	}

	t := target
	if !filepath.IsAbs(t) {
		if cwd == "" {
			return false
		}
		t = filepath.Join(cwd, t)
	}
	t = resolveExistingAncestor(filepath.Clean(t))
	tsegs := strings.Split(t, string(filepath.Separator))

	for _, g := range globs {
		if g == "" {
			continue
		}
		p := config.ExpandHome(g)
		if !filepath.IsAbs(p) {
			if cwd == "" {
				continue
			}
			p = filepath.Join(cwd, p)
		}
		// Resolve symlinks on the pattern's literal prefix so it is compared in the
		// same namespace as the symlink-resolved target (e.g. macOS /var ->
		// /private/var). The glob remainder (**, *, ?) does not exist as real files,
		// so resolveExistingAncestor leaves it untouched as the non-existent tail.
		p = resolveExistingAncestor(filepath.Clean(p))
		if matchSegments(strings.Split(p, string(filepath.Separator)), tsegs) {
			return true
		}
	}
	return false
}

// matchSegments reports whether the path segments in name match the pattern
// segments in pat, anchored end-to-end. A `**` pattern segment matches any number
// of path segments including zero; every other pattern segment is matched against
// a single path segment with filepath.Match (so `*` and `?` never cross a
// separator). Both slices come from splitting a cleaned absolute path on the OS
// separator, so their leading empty element (the root) lines up.
func matchSegments(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		// ** consumes zero or more segments: try every split point.
		for i := 0; i <= len(name); i++ {
			if matchSegments(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, err := filepath.Match(pat[0], name[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], name[1:])
}

// resolveExistingAncestor resolves symlinks on the longest existing prefix of p
// and rejoins the non-existent remainder, so a not-yet-created file still has its
// real parent directory resolved (a symlinked ancestor cannot mask an escape).
func resolveExistingAncestor(p string) string {
	rest := ""
	cur := p
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return real
			}
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // reached the root without resolving anything
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Decision is the outcome of classifying one tool call or command.
type Decision struct {
	Allow  bool
	Reason string // why it was denied (empty when allowed)
}

// Options configure how a fence is applied to a launch.
type Options struct {
	// HookCmd is the PreToolUse command the harness runs to classify each tool
	// call, e.g. "/abs/path/ax fence-check".
	HookCmd string
	// WriteGlobs is the write scope: the set of path globs a fenced session may
	// write into. A non-empty scope drops the file-write tools from the settings
	// deny list so the hook can allow them for a matching path (everything else
	// stays blocked); an empty scope is the --no-write case, where the file-write
	// tools stay denied and the session writes nothing.
	WriteGlobs []string
	// NoSubagents, when true, adds the sub-agent spawn tools (Task/TaskCreate/
	// Agent) to the settings deny-list backstop, so a session that must delegate
	// via `ax claude ...` cannot spawn them. Set by userland (e.g. a role recipe).
	// Core is role-agnostic: it knows this capability, not any role name.
	NoSubagents bool
}

// Apply returns the harness-specific launch flags that install the read-only
// fence, plus the settings JSON it built (for logging/inspection). It returns
// ErrUnsupported for a harness ax cannot fence, so the caller can fail closed.
func Apply(format string, opts Options) (extraArgs []string, settingsJSON string, err error) {
	switch format {
	case "claude":
		settings := map[string]any{
			"permissions": map[string]any{"deny": deniedToolsFor(len(opts.WriteGlobs) > 0, opts.NoSubagents)},
			"hooks": map[string]any{
				"PreToolUse": []any{
					map[string]any{
						"matcher": "*",
						"hooks": []any{
							map[string]any{"type": "command", "command": opts.HookCmd},
						},
					},
				},
			},
		}
		b, err := json.Marshal(settings)
		if err != nil {
			return nil, "", err
		}
		return []string{"--settings", string(b)}, string(b), nil
	default:
		// codex, pi, opencode: no settings/hook mechanism ax can drive to enforce
		// a read-only allowlist. Fail closed.
		return nil, "", ErrUnsupported
	}
}

var readCoreutils = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "rg": true, "jq": true, "echo": true,
	"test": true, "true": true, "uniq": true, "cut": true,
	"tr": true, "column": true, "which": true,
	"printf": true, "cd": true,
	// sed is handled specially: allowed only without an in-place flag.
	// command is handled specially: only `command -v` (a read-only lookup) is
	// allowed, since `command <cmd>` executes its argument.
	// env is handled specially: it can exec a command, so only assignments and
	// flags are allowed after it, never a bare command word.
	// find is handled specially: its -delete/-exec/-execdir/-fprint* action
	// flags write or run commands, so only a plain search is allowed.
	// awk is handled specially: system() and print redirection/pipes run or
	// write, so only a pure read script is allowed.
	// sort is handled specially: -o/--output writes its result to a file.
	// cd is read-only navigation: it cannot create or modify files.
	// printf writes only to stdout (a real-file redirect is caught separately).
}

var gitRead = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true,
	"branch": true, "ls-files": true, "rev-parse": true,
	"cat-file": true, "rev-list": true, "for-each-ref": true, "show-ref": true,
	"describe": true, "shortlog": true, "reflog": true, "blame": true,
	"ls-tree": true, "grep": true,
	// remote, config and tag are handled specially: they have both read and
	// write forms, so only the read forms are allowed.
}

var gitWrite = map[string]bool{
	"commit": true, "push": true, "add": true, "checkout": true,
	"reset": true, "merge": true,
}

// Classify decides whether a Bash command line is read-only, with no extra
// allowed commands. It is the deny-by-default allowlist the fence-check hook
// runs over each pipeline segment.
func Classify(cmd string) Decision { return ClassifyWith(cmd, nil) }

// ClassifyWith is Classify with extra allowed leading commands (the config
// [fence] allow list). A command is allowed only if EVERY pipeline segment is
// allowed; the first denied segment fails the whole line.
func ClassifyWith(cmd string, allowExec []string) Decision {
	for _, seg := range splitSegments(cmd) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if d := classifySegment(seg, allowExec); !d.Allow {
			return d
		}
	}
	return Decision{Allow: true}
}

func classifySegment(seg string, allowExec []string) Decision {
	// A real file-write redirect makes any command a write, whatever the program
	// is. But a `>` inside quotes is literal text, a file-descriptor duplication
	// (2>&1, >&2, N>&M) is not a file write, and a redirect to /dev/null or the
	// std streams writes nothing durable, so those are allowed.
	if d := checkRedirects(seg); !d.Allow {
		return d
	}
	seg = stripRedirects(seg)
	fields := strings.Fields(seg)
	for len(fields) > 0 && isAssignment(fields[0]) {
		fields = fields[1:] // skip leading VAR=val prefixes
	}
	if len(fields) == 0 {
		return Decision{Allow: true}
	}
	exec := normalizeExec(fields[0])

	// ax drives its own fences (depth, workers, policy, the accept check); running it
	// is always allowed, which is what lets a fenced session keep driving ax.
	if exec == "ax" {
		return Decision{Allow: true}
	}
	if exec == "git" {
		return classifyGit(fields[1:])
	}
	if exec == "sed" {
		for _, f := range fields[1:] {
			if f == "-i" || strings.HasPrefix(f, "-i") || f == "--in-place" || strings.HasPrefix(f, "--in-place") {
				return deny("sed -i edits files in place")
			}
		}
		return Decision{Allow: true}
	}
	if exec == "command" {
		// `command -v/-V name` prints where name resolves (read-only); `command
		// name args...` runs name, so only the lookup form is allowed.
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "-") && (strings.ContainsRune(f, 'v') || strings.ContainsRune(f, 'V')) {
				return Decision{Allow: true}
			}
		}
		return deny("`command` runs its argument; only `command -v` (a read-only lookup) is allowed")
	}
	if exec == "env" {
		// `env` sets variables then execs a command; only the pure-environment
		// form (assignments and its own flags, e.g. -i / -u NAME / -0) is
		// read-only. A bare command word after env means it will run that
		// command, so deny.
		args := fields[1:]
		for i := 0; i < len(args); i++ {
			a := args[i]
			if isAssignment(a) {
				continue
			}
			if strings.HasPrefix(a, "-") {
				if a == "-u" {
					i++ // -u takes a NAME argument; skip it
				}
				continue
			}
			return deny("`env` runs the command that follows; only assignments and flags are read-only")
		}
		return Decision{Allow: true}
	}
	if exec == "find" {
		// find's action flags run commands or write files; only a read-only
		// search (name/path/type tests, -print) is allowed.
		for _, f := range fields[1:] {
			switch f {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir",
				"-fprint", "-fprint0", "-fprintf", "-fls":
				return deny("`find " + f + "` writes files or runs commands; only a read-only find is allowed")
			}
		}
		return Decision{Allow: true}
	}
	if exec == "awk" {
		// awk can call system() (runs a shell command) and redirect or pipe
		// print output to a file/command; only a pure read script is allowed.
		prog := strings.Join(fields[1:], " ")
		if awkSystemRe.MatchString(prog) {
			return deny("`awk` calls system(), which runs a command")
		}
		if awkRedirRe.MatchString(prog) {
			return deny("`awk` redirects or pipes print output, which writes a file or runs a command")
		}
		return Decision{Allow: true}
	}
	if exec == "sort" {
		// sort -o / --output writes the sorted result to a file.
		for _, f := range fields[1:] {
			if f == "-o" || strings.HasPrefix(f, "-o") || f == "--output" || strings.HasPrefix(f, "--output") {
				return deny("`sort -o` writes its output to a file")
			}
		}
		return Decision{Allow: true}
	}
	if readCoreutils[exec] {
		return Decision{Allow: true}
	}
	for _, a := range allowExec {
		if exec == a {
			return Decision{Allow: true}
		}
	}
	return deny("`" + exec + "` is not on the read-only allowlist; delegate writes to a worker")
}

// classifyGit allows git's read subcommands and refuses its writes. rest is the
// tokens after "git".
func classifyGit(rest []string) Decision {
	i := 0
	for i < len(rest) {
		f := rest[i]
		// Global flags that take a value, so the subcommand is not their argument.
		if f == "-C" || f == "-c" || f == "--git-dir" || f == "--work-tree" {
			i += 2
			continue
		}
		if strings.HasPrefix(f, "-") {
			i++
			continue
		}
		break
	}
	if i >= len(rest) {
		return Decision{Allow: true} // bare `git` prints help
	}
	sub := rest[i]
	if gitWrite[sub] {
		return deny("`git " + sub + "` is a write")
	}
	if sub == "worktree" {
		for _, f := range rest[i+1:] {
			if strings.HasPrefix(f, "-") {
				continue
			}
			if f == "list" {
				return Decision{Allow: true}
			}
			return deny("`git worktree " + f + "` is a write; only `git worktree list` is read-only")
		}
		return deny("`git worktree` needs a read-only subcommand (list)")
	}
	if sub == "remote" {
		// Read forms: bare, -v, `show`, `get-url`. Writes (add/remove/rename/
		// set-url) all name a bare subcommand word, so deny any bare word that is
		// not a read.
		for _, f := range rest[i+1:] {
			if strings.HasPrefix(f, "-") {
				continue // -v and friends
			}
			if f == "show" || f == "get-url" {
				return Decision{Allow: true}
			}
			return deny("`git remote " + f + "` is a write; only read forms (bare, -v, show, get-url) are allowed")
		}
		return Decision{Allow: true} // bare `git remote` or just `-v`
	}
	if sub == "config" {
		// Only the read forms are allowed; a set is a KEY VALUE pair or a
		// mutating flag (--add/--unset/--replace-all), none of which name a read
		// flag.
		for _, f := range rest[i+1:] {
			switch f {
			case "--get", "--get-all", "--get-regexp", "--list", "-l":
				return Decision{Allow: true}
			}
		}
		return deny("`git config` is a write here; only reads (--get/--get-all/--list/-l) are allowed")
	}
	if sub == "tag" {
		// List-only: bare, -l/--list, -n. A tag name (a bare word) means create,
		// and -d/-a with a name is delete/annotate, so any bare word is denied.
		for _, f := range rest[i+1:] {
			if strings.HasPrefix(f, "-") {
				continue // -l, --list, -n, -n5
			}
			return deny("`git tag " + f + "` creates or deletes a tag; only listing (bare, -l, --list, -n) is allowed")
		}
		return Decision{Allow: true}
	}
	if gitRead[sub] {
		return Decision{Allow: true}
	}
	return deny("`git " + sub + "` is not on the read-only allowlist")
}

func deny(reason string) Decision { return Decision{Allow: false, Reason: reason} }

var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// awkSystemRe matches an awk system(...) call; awkRedirRe matches a print/printf
// followed by a redirect (>, >>) or pipe (|), i.e. output sent to a file or
// command. Both make an awk program a write/exec rather than a pure read.
var awkSystemRe = regexp.MustCompile(`system\s*\(`)
var awkRedirRe = regexp.MustCompile(`\b(print|printf)\b[^{};]*[>|]`)

func isAssignment(tok string) bool { return assignRe.MatchString(tok) }

// normalizeExec reduces a leading token to a bare program name: it trims
// surrounding quotes, maps the $AX self-reference to ax, and takes the basename
// of a path, so /usr/bin/git, "git" and git all classify the same.
func normalizeExec(tok string) string {
	tok = strings.Trim(tok, `"'`)
	switch tok {
	case "$AX", "${AX}":
		return "ax"
	}
	if i := strings.LastIndexByte(tok, '/'); i >= 0 {
		tok = tok[i+1:]
	}
	return tok
}

// splitSegments breaks a command line into its simple-command segments on the
// shell operators ; && || | & and newlines, so each is classified on its own.
// It is quote-aware: an operator (or newline) that sits inside single OR double
// quotes is literal text, not a separator, so a multi-line double-quoted
// argument (e.g. a task string passed to ax) stays a single segment instead of
// being shattered into phantom segments. A `&` that immediately follows a `>`
// is part of a file-descriptor redirect (2>&1, >&2), not a background operator,
// so it does not split either.
func splitSegments(s string) []string {
	var segs []string
	var cur strings.Builder
	var last rune // last rune written to cur (0 after a flush)
	runes := []rune(s)
	flush := func() {
		segs = append(segs, cur.String())
		cur.Reset()
		last = 0
	}
	write := func(r rune) { cur.WriteRune(r); last = r }
	var inS, inD bool
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\\' && !inS:
			// A backslash escapes the next char (outside single quotes, where
			// it is literal). The escaped char -- notably \" or \' -- is a
			// literal: it must not toggle quote state, and crucially it must
			// not count as `last` for operator adjacency. An escaped `\>` is
			// not a redirection and cannot form a `>&` fd-duplication, so a
			// following `&` must still split. Clear last so the `last == '>'`
			// guard below can't fire.
			cur.WriteRune(r)
			if i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
			}
			last = 0
			continue
		case r == '\'' && !inD:
			inS = !inS
			write(r)
			continue
		case r == '"' && !inS:
			inD = !inD
			write(r)
			continue
		}
		if inS || inD {
			write(r)
			continue
		}
		switch r {
		case '\n', ';':
			flush()
		case '&':
			if last == '>' { // part of a >& fd duplication, not an operator
				write(r)
				continue
			}
			if i+1 < len(runes) && runes[i+1] == '&' {
				i++
			}
			flush()
		case '|':
			if i+1 < len(runes) && runes[i+1] == '|' {
				i++
			}
			flush()
		default:
			write(r)
		}
	}
	flush()
	return segs
}

// checkRedirects denies a segment that contains a real file-write redirect. It
// scans quote-aware, so a `>` inside single or double quotes is literal text.
// Outside quotes, a `>` or `>>` is inspected: a file-descriptor duplication or
// close (>&N, N>&M, >&-) is not a file write, and a redirect whose target is
// /dev/null or the std streams writes nothing durable, so both are allowed.
// Any other target (including an append to a real file) is a write and denied.
func checkRedirects(seg string) Decision {
	runes := []rune(seg)
	var inS, inD bool
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\\' && !inS:
			// A backslash escapes the next char (outside single quotes). The
			// escaped char -- a \" or \' or even a literal \> -- cannot toggle
			// quote state nor be a real redirect, so skip it.
			if i+1 < len(runes) {
				i++
			}
			continue
		case r == '\'' && !inD:
			inS = !inS
			continue
		case r == '"' && !inS:
			inD = !inD
			continue
		}
		if inS || inD || r != '>' {
			continue
		}
		j := i + 1
		if j < len(runes) && runes[j] == '>' {
			j++ // append form >>
		}
		if j < len(runes) && runes[j] == '&' {
			// >&word: a duplication/close if word is digits or '-', else it
			// redirects the stream to a file.
			tok := readToken(runes, j+1)
			if tok == "-" || isAllDigits(tok) {
				continue
			}
			return deny("redirect `>&" + tok + "` sends output to a file, which is a write")
		}
		k := j
		for k < len(runes) && (runes[k] == ' ' || runes[k] == '\t') {
			k++
		}
		target := strings.Trim(readToken(runes, k), `"'`)
		switch target {
		case "/dev/null", "/dev/stdout", "/dev/stderr":
			continue
		}
		return deny("output redirect (> / >>) to `" + target + "` is a write")
	}
	return Decision{Allow: true}
}

// stripRedirects returns seg with every redirect clause removed, quote-aware.
// It uses the same scan style as checkRedirects: unquoted > triggers removal
// of the whole clause (optional leading fd digits, the > or >>, and the
// target or &fd). This must be called only after checkRedirects has approved
// the segment, so the target is guaranteed to be /dev/null, a std stream, or
// an fd duplication -- never a real file write.
func stripRedirects(seg string) string {
	runes := []rune(seg)
	out := make([]rune, 0, len(runes))
	var inS, inD bool
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\\' && !inS:
			// A backslash escapes the next char (outside single quotes). Keep
			// both verbatim: the escaped char is literal, never a redirect, so
			// it must not toggle quote state nor be treated as a > to strip.
			out = append(out, r)
			if i+1 < len(runes) {
				i++
				out = append(out, runes[i])
			}
			continue
		case r == '\'' && !inD:
			inS = !inS
			out = append(out, r)
			continue
		case r == '"' && !inS:
			inD = !inD
			out = append(out, r)
			continue
		}
		if inS || inD || r != '>' {
			out = append(out, r)
			continue
		}
		// Unquoted >: strip any immediately-preceding fd digits from out
		// (e.g. the "2" in "2>&1"; a space before > means no fd digit to strip).
		for len(out) > 0 && out[len(out)-1] >= '0' && out[len(out)-1] <= '9' {
			out = out[:len(out)-1]
		}
		j := i + 1
		if j < len(runes) && runes[j] == '>' {
			j++ // >>
		}
		if j < len(runes) && runes[j] == '&' {
			j++ // &
			tok := readToken(runes, j)
			j += len([]rune(tok))
		} else {
			for j < len(runes) && (runes[j] == ' ' || runes[j] == '\t') {
				j++
			}
			tok := readToken(runes, j)
			j += len([]rune(tok))
		}
		// strip whitespace that preceded the redirect clause in out
		for len(out) > 0 && (out[len(out)-1] == ' ' || out[len(out)-1] == '\t') {
			out = out[:len(out)-1]
		}
		i = j - 1 // -1 because the loop increments i
	}
	return string(out)
}

// readToken returns the run of non-space, non-operator runes starting at i.
func readToken(runes []rune, i int) string {
	start := i
	for i < len(runes) {
		switch runes[i] {
		case ' ', '\t', '|', '&', ';', '<', '>':
			return string(runes[start:i])
		}
		i++
	}
	return string(runes[start:])
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
