// Package session reads LLM transcripts off disk into a uniform Session shape:
// harness, id, project dir, model, recency, and token/cost metrics.
package session

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/remap"
)

// Session is one resumable LLM conversation, normalized across harnesses.
type Session struct {
	Harness string
	Host    string // federation host label; "" = local (set by the access point)
	ID      string
	Dir     string
	Model   string
	Title   string
	File    string
	Last    time.Time
	Created time.Time // first transcript timestamp; used to match a hand-started window

	InTok       int // fresh input tokens, summed
	OutTok      int // output tokens, summed
	CacheReadT  int // cached input read, summed
	CacheWriteT int // cache creation, summed
	CtxTok      int // tokens loaded on the most recent turn (context fill)
	CtxWindow   int // context window size when the harness reports it (else 0)

	Cost    float64 // cumulative cost when the harness records it (pi)
	HasCost bool

	// Control-layer metadata, merged from the meta sidecar (not the transcript).
	Name              string    // display name; falls back to Title in the UI
	Task              string    // the prompt this session was launched with
	Group             string    // run/addressing label shared by a run's sessions
	Parent            string    // the session that spawned this one
	Origin            string    // "human" | "agent"
	Mode              string    // execution mode launched in: "interactive" | "headless"
	Effort            string    // reasoning effort level (low, medium, high, xhigh, max, ...)
	Outcome           string    // root-reported run result
	FailReason        string    // short reason a failed headless run was marked failed; "" unless Outcome == "failure"
	RecipePath        string    // tracked recipe script path; empty for harness sessions
	RecipeInterpreter []string  // argv prefix used to execute the recipe
	LogPath           string    // recipe stdout/stderr tee log
	Labels            []string  // free-form labels
	HasSpec           bool      // a persisted launch spec exists (session is `ax restart`-able)
	KeepLive          bool      // opted out of the post-conclude reap (--keep-live / --keep-live-for)
	KeepUntil         time.Time // keep-live lease deadline; zero with KeepLive is indefinite
	Archived          bool      // hidden from default session views, reversible via metadata
	ArchivedAt        time.Time
}

// Key is a session's access-point-wide identity: the bare id for a local
// session, or "host/id" for a remote one. It is what ax stores in the viewer
// window's @ax_session tag, so Locate/Focus can tell a local session from the
// same id federated off another machine.
func Key(s Session) string {
	if s.Host == "" {
		return s.ID
	}
	return s.Host + "/" + s.ID
}

// titleText filters harness-injected wrapper content out of a title candidate:
// a first "user" record that is a caveat block, a slash-command echo, or a
// system reminder is plumbing, not what the session is about.
func titleText(txt string) string {
	t := strings.TrimSpace(txt)
	for _, skip := range []string{
		"<local-command-", "Caveat:", "<command-name>", "<command-message>",
		"<system-reminder>", "<task-notification>",
	} {
		if strings.HasPrefix(t, skip) {
			return ""
		}
	}
	return t
}

// Index globs every configured harness and parses each transcript, newest
// first. Unreadable or unparseable files are skipped.
func Index(cfg config.Config) []Session {
	return index(cfg, true)
}

// IndexReadOnly returns the same sessions as Index without writing derived
// caches or sidecars. It is for dry-run paths that promise not to mutate state.
func IndexReadOnly(cfg config.Config) []Session {
	return index(cfg, false)
}

func index(cfg config.Config, persist bool) []Session {
	cache := loadIndexCache()
	fresh := map[string]cacheEntry{}
	dirty := false
	var out []Session
	for _, h := range cfg.Harnesses {
		if h.Format == "opencode" {
			for _, s := range indexOpencode(config.ExpandHome(h.DB), persist) {
				s.Harness = h.Name
				out = append(out, s)
			}
			continue
		}
		re, err := regexp.Compile(h.IDRe)
		if err != nil {
			continue
		}
		idIdx := re.SubexpIndex("id")
		matches, _ := filepath.Glob(config.ExpandHome(h.Glob))
		for _, path := range matches {
			st, err := os.Stat(path)
			if err != nil {
				continue
			}
			mt := st.ModTime().UnixNano()
			if e, ok := cache[path]; ok && e.MTime == mt {
				fresh[path] = e
				out = append(out, e.Sess)
				continue
			}
			// Match the id_regex against a forward-slash form of the path: the
			// built-in patterns anchor the id after a "/" separator, which a
			// native Windows glob result (backslashes) would never satisfy, so
			// every session went unindexed there. ToSlash is identity on unix.
			m := re.FindStringSubmatch(filepath.ToSlash(path))
			if m == nil || idIdx < 0 || idIdx >= len(m) {
				continue
			}
			s := ParseByFormat(h.Format, path)
			if s == nil {
				continue
			}
			s.Harness = h.Name
			if s.ID == "" {
				s.ID = m[idIdx]
			}
			out = append(out, *s)
			fresh[path] = cacheEntry{MTime: mt, Sess: *s}
			dirty = true // a transcript was (re)parsed
		}
	}
	// Rewrite the cache only when something changed: pollers (fence watchers,
	// group follows) call Index every few seconds, and marshalling hundreds of
	// KB to disk each tick for an identical cache is pure waste.
	if persist && (dirty || len(fresh) != len(cache)) {
		saveIndexCache(fresh)
	}
	// Merge the control-layer metadata sidecar (name/task/group/parent/...), read
	// once for the whole index. It is applied to the result, not the cache, so a
	// re-tag shows up without reparsing transcripts.
	if md := meta.LoadAll(); len(md) > 0 {
		have := map[string]bool{}
		for i := range out {
			have[out[i].ID] = true
			if m, ok := md[out[i].ID]; ok {
				out[i].Name, out[i].Task, out[i].Group = m.Name, m.Task, m.Group
				out[i].Parent, out[i].Origin, out[i].Outcome = m.Parent, m.Origin, m.Outcome
				out[i].FailReason = m.FailReason
				out[i].RecipePath = m.RecipePath
				out[i].RecipeInterpreter = m.RecipeInterpreter
				out[i].LogPath = m.LogPath
				out[i].Mode = m.Mode
				out[i].Effort = m.Effort
				out[i].Labels = m.Labels
				out[i].HasSpec = m.Spec != nil
				out[i].KeepLive = m.KeepLive
				out[i].KeepUntil = m.KeepUntil
				out[i].Archived = m.Archived
				out[i].ArchivedAt = m.ArchivedAt
			}
		}
		// A session ax launched but whose transcript is not written yet is invisible
		// to the glob; synthesize it from its metadata while it has a live heartbeat,
		// so a parent sees a child session the instant it launches. Recipe roots have
		// no harness transcript at all, so keep their meta-only row after conclusion
		// when the durable done/failed marker exists. A real parsed session replaces a
		// synthetic one once the transcript appears.
		beat := live.Snapshot()
		for id, m := range md {
			if have[id] {
				continue
			}
			if !metaOnlyVisible(id, m, beat) {
				continue
			}
			out = append(out, Session{
				ID: id, Harness: m.Harness, Dir: m.Dir, Name: m.Name, Task: m.Task,
				Group: m.Group, Parent: m.Parent, Origin: m.Origin, Mode: m.Mode, Effort: m.Effort,
				Outcome: m.Outcome, FailReason: m.FailReason,
				RecipePath: m.RecipePath, RecipeInterpreter: m.RecipeInterpreter, LogPath: m.LogPath,
				Labels: m.Labels, HasSpec: m.Spec != nil, Archived: m.Archived, ArchivedAt: m.ArchivedAt,
				KeepLive: m.KeepLive, KeepUntil: m.KeepUntil,
				Last: m.Updated, Created: m.Updated,
			})
		}
	}
	// Relink sessions whose recorded folder was renamed or moved. The map is
	// keyed by directory, so one confirmed relink covers every session there.
	// Applied on the final result (not the cache) so it stays current, and after
	// the meta merge so meta-synthesized sessions relink too.
	if rm := remap.Load(); len(rm) > 0 {
		for i := range out {
			if nd, ok := rm[out[i].Dir]; ok {
				out[i].Dir = nd
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

func metaOnlyVisible(id string, m meta.Meta, beat map[string]live.Entry) bool {
	if e, ok := beat[id]; ok && live.Running(e) {
		return true
	}
	return m.Mode == "recipe" && terminalHook(id)
}

func terminalHook(id string) bool {
	data, err := os.ReadFile(filepath.Join(axdir.StatePath("hookstate"), id))
	if err != nil {
		return false
	}
	switch strings.TrimSpace(string(data)) {
	case "done", "failed":
		return true
	default:
		return false
	}
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// extractText pulls readable text out of a message content field that may be a
// plain string or an array of typed blocks.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return strings.TrimSpace(str)
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		// First block carrying text. Covers claude's {type:text}, codex's
		// {type:input_text/output_text}, and similar shapes; tool-call blocks
		// have no text and are skipped.
		for _, b := range blocks {
			if strings.TrimSpace(b.Text) != "" {
				return strings.TrimSpace(b.Text)
			}
		}
	}
	return ""
}

type claudeRec struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Cwd       string          `json:"cwd"`
	AiTitle   string          `json:"aiTitle"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type claudeMsg struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   struct {
		Input      int `json:"input_tokens"`
		Output     int `json:"output_tokens"`
		CacheRead  int `json:"cache_read_input_tokens"`
		CacheWrite int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func parseClaude(path string) *Session {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	s := &Session{File: path, Dir: dirFromMangled(path)}
	// Keep the first cwd, not the last: it is the dir the session started in (the
	// project claude filed it under), which is what `claude --resume` needs. A
	// session that later cd's elsewhere would otherwise resume in the wrong project
	// and fail with "no conversation found".
	dirSet := false
	// Claude re-logs the same assistant message (same id and usage) more than once,
	// so token totals must accumulate each message id only once or cost inflates
	// (observed ~2x). Context is the last turn's size, so it is dup-safe already.
	seenMsg := map[string]bool{}
	dec := json.NewDecoder(f)
	for {
		var r claudeRec
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			// one bad record shouldn't sink the whole file
			if !skipBadToken(dec) {
				break
			}
			continue
		}
		if r.SessionID != "" {
			s.ID = r.SessionID
		}
		if r.Cwd != "" && !dirSet {
			s.Dir = r.Cwd
			dirSet = true
		}
		if r.AiTitle != "" {
			s.Title = r.AiTitle
		}
		if t := parseTime(r.Timestamp); !t.IsZero() {
			if t.After(s.Last) {
				s.Last = t
			}
			if s.Created.IsZero() || t.Before(s.Created) {
				s.Created = t
			}
		}
		if len(r.Message) == 0 {
			continue
		}
		var m claudeMsg
		if json.Unmarshal(r.Message, &m) != nil {
			continue
		}
		if r.Type == "user" && s.Title == "" {
			if txt := titleText(extractText(m.Content)); txt != "" {
				s.Title = txt
			}
		}
		if r.Type == "assistant" {
			// Skip placeholder models like "<synthetic>" (harness-injected error
			// messages carry it): recording one would ride into `--resume --model
			// <synthetic>` and kill the resume. Keep the last real model instead.
			if m.Model != "" && !strings.HasPrefix(m.Model, "<") {
				s.Model = m.Model
			}
			if ctx := m.Usage.Input + m.Usage.CacheRead + m.Usage.CacheWrite; ctx > 0 {
				s.CtxTok = ctx // last non-zero turn wins (dup-safe: dupes are identical)
			}
			// Count each assistant message once. A record with no id (rare) is always
			// counted; a repeated id is a re-log of an already-counted message.
			if m.ID == "" || !seenMsg[m.ID] {
				seenMsg[m.ID] = true
				s.InTok += m.Usage.Input
				s.OutTok += m.Usage.Output
				s.CacheReadT += m.Usage.CacheRead
				s.CacheWriteT += m.Usage.CacheWrite
			}
		}
	}
	return s
}

type piRec struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Cwd       string          `json:"cwd"`
	Timestamp string          `json:"timestamp"`
	ModelID   string          `json:"modelId"`
	Message   json.RawMessage `json:"message"`
}

type piMsg struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   struct {
		Input      int `json:"input"`
		Output     int `json:"output"`
		CacheRead  int `json:"cacheRead"`
		CacheWrite int `json:"cacheWrite"`
		Total      int `json:"totalTokens"`
		Cost       struct {
			Total float64 `json:"total"`
		} `json:"cost"`
	} `json:"usage"`
}

func parsePi(path string) *Session {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	s := &Session{File: path, Dir: dirFromMangled(path), HasCost: true}
	dirSet := false // keep the first cwd (the project dir), as for claude above
	dec := json.NewDecoder(f)
	for {
		var r piRec
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			if !skipBadToken(dec) {
				break
			}
			continue
		}
		switch r.Type {
		case "session":
			if r.ID != "" {
				s.ID = r.ID
			}
			if r.Cwd != "" && !dirSet {
				s.Dir = r.Cwd
				dirSet = true
			}
		case "model_change":
			if r.ModelID != "" {
				s.Model = r.ModelID
			}
		}
		if t := parseTime(r.Timestamp); !t.IsZero() {
			if t.After(s.Last) {
				s.Last = t
			}
			if s.Created.IsZero() || t.Before(s.Created) {
				s.Created = t
			}
		}
		if r.Type != "message" || len(r.Message) == 0 {
			continue
		}
		var m piMsg
		if json.Unmarshal(r.Message, &m) != nil {
			continue
		}
		if m.Model != "" {
			s.Model = m.Model
		}
		if m.Role == "user" && s.Title == "" {
			if txt := titleText(extractText(m.Content)); txt != "" {
				s.Title = txt
			}
		}
		s.InTok += m.Usage.Input
		s.OutTok += m.Usage.Output
		s.CacheReadT += m.Usage.CacheRead
		s.CacheWriteT += m.Usage.CacheWrite
		s.Cost += m.Usage.Cost.Total
		if m.Usage.Input+m.Usage.CacheRead > 0 {
			s.CtxTok = m.Usage.Input + m.Usage.CacheRead
		}
	}
	return s
}

type codexRec struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type codexTokUsage struct {
	Input  int `json:"input_tokens"`
	Cached int `json:"cached_input_tokens"`
	Output int `json:"output_tokens"`
}

func parseCodex(path string) *Session {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	s := &Session{File: path}
	dirSet := false // keep the first cwd (the project dir), as for claude/pi above
	dec := json.NewDecoder(f)
	for {
		var r codexRec
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			if !skipBadToken(dec) {
				break
			}
			continue
		}
		if t := parseTime(r.Timestamp); !t.IsZero() {
			if t.After(s.Last) {
				s.Last = t
			}
			if s.Created.IsZero() || t.Before(s.Created) {
				s.Created = t
			}
		}
		if len(r.Payload) == 0 {
			continue
		}
		switch r.Type {
		case "session_meta":
			var p struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(r.Payload, &p) == nil {
				if p.ID != "" {
					s.ID = p.ID
				}
				if p.Cwd != "" && !dirSet {
					s.Dir = p.Cwd
					dirSet = true
				}
			}
		case "turn_context":
			var p struct {
				Model string `json:"model"`
				Cwd   string `json:"cwd"`
			}
			if json.Unmarshal(r.Payload, &p) == nil {
				if p.Model != "" {
					s.Model = p.Model
				}
				if p.Cwd != "" && !dirSet {
					s.Dir = p.Cwd
					dirSet = true
				}
			}
		case "event_msg":
			var p struct {
				Type      string `json:"type"`
				CtxWindow int    `json:"model_context_window"`
				Info      struct {
					Total codexTokUsage `json:"total_token_usage"`
					Last  codexTokUsage `json:"last_token_usage"`
				} `json:"info"`
			}
			if json.Unmarshal(r.Payload, &p) != nil {
				continue
			}
			if p.Type == "task_started" && p.CtxWindow > 0 {
				s.CtxWindow = p.CtxWindow
			}
			if p.Type == "token_count" {
				if in := p.Info.Total.Input - p.Info.Total.Cached; in > 0 {
					s.InTok = in
				}
				s.CacheReadT = p.Info.Total.Cached
				s.OutTok = p.Info.Total.Output
				s.CtxTok = p.Info.Last.Input
			}
		case "response_item":
			if s.Title != "" {
				continue
			}
			var p struct {
				Type    string          `json:"type"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(r.Payload, &p) == nil && p.Type == "message" && p.Role == "user" {
				// codex injects wrapper messages (<environment_context>,
				// <user_instructions>, ...) as the first user turns; the real
				// prompt is the first that isn't an XML-tagged block.
				if txt := titleText(extractText(p.Content)); txt != "" && !strings.HasPrefix(txt, "<") {
					s.Title = txt
				}
			}
		}
	}
	return s
}

// ParseByFormat parses a single transcript with the named format parser.
func ParseByFormat(format, path string) *Session {
	switch format {
	case "claude":
		return parseClaude(path)
	case "pi":
		return parsePi(path)
	case "codex":
		return parseCodex(path)
	}
	return nil
}

// Turn is one message in a transcript, for preview rendering.
type Turn struct {
	Role string
	Text string
	Time time.Time
}

// RecentText returns up to the last n user/assistant turns with readable text.
func RecentText(format, path string, n int) []Turn {
	turns := allTurns(format, path)
	if len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	return turns
}

// FullText concatenates every readable turn, for the content-search sidecar.
func FullText(format, path string) string {
	var b strings.Builder
	for _, t := range allTurns(format, path) {
		b.WriteString(t.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func allTurns(format, path string) []Turn {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var turns []Turn
	dec := json.NewDecoder(f)
	for {
		var raw struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Message   json.RawMessage `json:"message"`
			Payload   json.RawMessage `json:"payload"`
		}
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF || !skipBadToken(dec) {
				break
			}
			continue
		}
		var role string
		var content json.RawMessage
		switch format {
		case "claude":
			if raw.Type != "user" && raw.Type != "assistant" {
				continue
			}
			role = raw.Type
			var m struct {
				Content json.RawMessage `json:"content"`
			}
			json.Unmarshal(raw.Message, &m)
			content = m.Content
		case "pi":
			if raw.Type != "message" {
				continue
			}
			var m struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			json.Unmarshal(raw.Message, &m)
			role = m.Role
			content = m.Content
		case "codex":
			if raw.Type != "response_item" {
				continue
			}
			var p struct {
				Type    string          `json:"type"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			json.Unmarshal(raw.Payload, &p)
			if p.Type != "message" || (p.Role != "user" && p.Role != "assistant") {
				continue
			}
			role = p.Role
			content = p.Content
		default:
			continue
		}
		if txt := extractText(content); txt != "" {
			turns = append(turns, Turn{Role: role, Text: txt, Time: parseTime(raw.Timestamp)})
		}
	}
	return turns
}

// skipBadToken advances the decoder past one offending token so a single
// malformed record doesn't abort the whole transcript. Returns false at EOF.
func skipBadToken(dec *json.Decoder) bool {
	_, err := dec.Token()
	return err == nil
}

// mangleRe matches a path-mangled project directory name like
// "-Users-alice-src-foo".
var mangleRe = regexp.MustCompile(`^-`)

// dirFromMangled recovers a best-effort project path from the parent directory
// name when the transcript itself carries no cwd. Dashes inside real directory
// names are ambiguous; the cwd field, when present, always wins upstream.
func dirFromMangled(path string) string {
	name := filepath.Base(filepath.Dir(path))
	if !mangleRe.MatchString(name) {
		return ""
	}
	return strings.ReplaceAll(name, "-", "/")
}
