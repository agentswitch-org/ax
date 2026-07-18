// Package view renders sessions for the picker: one row's columns, the column
// header with sort indicators, the top navbar, the preview, and the help screen.
// It is pure (no fzf/tmux/rg) and the finder TUI composes these primitives. The
// only outside touch is reading the terminal size (help) and transcripts
// (preview). Columns are a registry, so which to show and in what order is
// config-driven.
package view

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agentswitch-org/ax/internal/ask"
	"github.com/agentswitch-org/ax/internal/build"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/models"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
	"github.com/agentswitch-org/ax/internal/textcache"
	"github.com/mattn/go-runewidth"
)

// RowMeta is the per-session live state the picker needs but the transcript does
// not carry: where it runs in the multiplexer and whether it is live, crashed,
// or inactive (and, while live, working or idle). The finder computes it.
type RowMeta struct {
	Locator  string // "session:window.pane" when running in the multiplexer
	State    string // StateLive, StateCrash, or "" (inactive)
	Activity string // Working, Idle, or "" (only set for live sessions)
	DirGone  bool   // recorded project folder no longer exists (renamed/moved)
	// Control-layer display state (see internal/finder tree/roll-up).
	TreePrefix   string // run-tree glyphs for the name column in tree mode
	Depth        int    // depth in the run tree (0 = root)
	Waiting      string // "input" (needs you), "auth" (needs auth), or "children" (ax wait), else ""
	Lifecycle    string // live, concluded, crashed, or dormant
	DisplayPhase string // live-working, live-waiting, live-done-resident, concluded/crashed/dormant
	Archived     bool   // hidden from default views unless the archive tier is shown
	Yolo         bool   // running without guardrails (a --dangerously-* flag)
	Done         bool   // a task-carrying worker whose task concluded (halted in the done state)
	Failed       bool   // a headless run that errored (non-zero exit or a fatal output pattern)
	FailReason   string // short reason captured for a failed run; "" until Failed
	Detached     bool   // live but with no viewer window on this machine (a backgrounded worker)
}

// These re-export the canonical state values so view's renderers keep their
// short names while state owns the definitions (also serialized over the wire).
const (
	StateLive  = state.Live  // fresh heartbeat or an open window: running now
	StateCrash = state.Crash // a stale heartbeat: was running, died (recovery candidate)

	Working = state.Working // produced terminal output recently
	Idle    = state.Idle    // live but quiet (waiting for the user)

	PhaseLiveWorking      = state.DisplayLiveWorking
	PhaseLiveWaiting      = state.DisplayLiveWaiting
	PhaseLiveDoneResident = state.DisplayLiveDoneResident
	PhaseConcluded        = state.DisplayConcluded
)

// spinner frames animate the working indicator.
var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Categorical hues (256-color, theme-stable): hue answers "which kind", so the
// harness and tag columns become preattentive instead of a wall of one tone.
// Known harnesses get fixed hues; anything else hashes into the cycle.
var harnessHues = map[string]string{
	"claude":   "38;5;215", // peach
	"pi":       "38;5;140", // purple
	"codex":    "38;5;114", // green
	"opencode": "38;5;75",  // blue
}

var hueCycle = []string{"38;5;215", "38;5;140", "38;5;114", "38;5;75", "38;5;210", "38;5;80"}

func hueFor(name string) string {
	if h, ok := harnessHues[name]; ok {
		return h
	}
	sum := 0
	for _, r := range name {
		sum = sum*31 + int(r)
	}
	if sum < 0 {
		sum = -sum
	}
	return hueCycle[sum%len(hueCycle)]
}

// ageColor is the recency gradient: fresh work glows, stale work recedes.
// Brightness carries time; hue is reserved for categories.
func ageColor(t time.Time, s string) string {
	switch d := time.Since(t); {
	case d < time.Hour:
		return ansi("32", s) // green: active this hour
	case d < 24*time.Hour:
		return s
	case d < 7*24*time.Hour:
		return ansi("2", s)
	default:
		return ansi("38;5;242", s) // weeks old: nearly gone
	}
}

// column is one selectable table column.
type column struct {
	key   string
	label string
	width int  // display width; the last shown column is rendered unpadded
	desc  bool // default sort direction on first selection
	cell  func(s session.Session, db models.DB, m RowMeta, width, frame int) string
	less  func(a, b session.Session, db models.DB, meta map[string]RowMeta) bool
}

// registry is every available column. Config picks a subset and order.
var registry = []column{
	{"host", "HOST", 8, false,
		// Local is the default case and renders silent, so the one remote row
		// pops instead of hiding in a column of repeated "local".
		func(s session.Session, _ models.DB, _ RowMeta, w, _ int) string {
			if s.Host == "" {
				return ""
			}
			return clip(s.Host, w)
		},
		func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool {
			return hostLabel(a.Host) < hostLabel(b.Host)
		}},
	{"status", "STATUS", 10, false,
		// One cell for the one fact: needs you > working > idle > crash > gone.
		// (working implies live, so a separate STATE column was pure redundancy.)
		func(_ session.Session, _ models.DB, m RowMeta, _, frame int) string { return statusCell(m, frame) },
		func(a, b session.Session, _ models.DB, meta map[string]RowMeta) bool {
			return statusRank(meta[session.Key(a)]) < statusRank(meta[session.Key(b)])
		}},
	{"lifecycle", "LIFE", 13, false,
		func(_ session.Session, _ models.DB, m RowMeta, _, _ int) string { return lifecycleCell(displayLife(m)) },
		func(a, b session.Session, _ models.DB, meta map[string]RowMeta) bool {
			return lifecycleRank(displayLife(meta[session.Key(a)])) < lifecycleRank(displayLife(meta[session.Key(b)]))
		}},
	{"archived", "ARCH", 4, false,
		func(_ session.Session, _ models.DB, m RowMeta, _, _ int) string {
			if m.Archived {
				return ansi("2", "yes")
			}
			return ""
		},
		func(a, b session.Session, _ models.DB, meta map[string]RowMeta) bool {
			return meta[session.Key(a)].Archived && !meta[session.Key(b)].Archived
		}},
	{"harness", "HARNESS", 8, false,
		func(s session.Session, _ models.DB, _ RowMeta, _, _ int) string {
			return ansi(hueFor(s.Harness), s.Harness)
		},
		func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool { return a.Harness < b.Harness }},
	{"state", "STATE", 5, false,
		func(_ session.Session, _ models.DB, m RowMeta, _, _ int) string { return stateCell(m.State) },
		func(a, b session.Session, _ models.DB, meta map[string]RowMeta) bool {
			return stateRank(meta[session.Key(a)].State) < stateRank(meta[session.Key(b)].State)
		}},
	{"spin", "", 1, false,
		func(_ session.Session, _ models.DB, m RowMeta, _, frame int) string {
			return spinCell(m.Activity, frame)
		},
		func(a, b session.Session, _ models.DB, meta map[string]RowMeta) bool {
			return activityRank(meta[session.Key(a)].Activity) < activityRank(meta[session.Key(b)].Activity)
		}},
	{"activity", "ACT", 10, false,
		func(_ session.Session, _ models.DB, m RowMeta, _, _ int) string { return activityCell(m) },
		func(a, b session.Session, _ models.DB, meta map[string]RowMeta) bool {
			return activityRank(meta[session.Key(a)].Activity) < activityRank(meta[session.Key(b)].Activity)
		}},
	{"yolo", "", 1, false,
		func(_ session.Session, _ models.DB, m RowMeta, _, _ int) string {
			if m.Yolo {
				return ansi("1;33", "⚠︎") // U+FE0E: narrow text form, not the wide emoji
			}
			return ""
		},
		func(a, b session.Session, _ models.DB, meta map[string]RowMeta) bool {
			return meta[session.Key(a)].Yolo && !meta[session.Key(b)].Yolo // unsandboxed first
		}},
	{"name", "NAME", 22, false,
		func(s session.Session, _ models.DB, m RowMeta, w, _ int) string {
			return clip(m.TreePrefix+displayName(s), w)
		},
		func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool {
			return displayName(a) < displayName(b)
		}},
	{"run", "RUN", 10, false,
		func(s session.Session, _ models.DB, _ RowMeta, _, _ int) string { return s.Group },
		func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool { return a.Group < b.Group }},
	{"tags", "TAGS", 18, false,
		func(s session.Session, _ models.DB, _ RowMeta, w, _ int) string { return tagsCell(s.Labels, w) },
		func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool {
			return strings.Join(a.Labels, " ") < strings.Join(b.Labels, " ")
		}},
	{"model", "MODEL", 12, false,
		func(s session.Session, _ models.DB, _ RowMeta, _, _ int) string {
			m := shortModel(s.Model)
			if m == "?" || strings.HasPrefix(m, "<") { // unknown / synthetic: noise, dim it
				return ansi("2", m)
			}
			return m
		},
		func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool {
			return shortModel(a.Model) < shortModel(b.Model)
		}},
	{"effort", "EFF", 6, false,
		func(s session.Session, _ models.DB, _ RowMeta, _, _ int) string {
			if s.Effort == "" {
				return ""
			}
			return s.Effort
		},
		func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool {
			return a.Effort < b.Effort
		}},
	{"age", "AGE", 4, true,
		func(s session.Session, _ models.DB, _ RowMeta, _, _ int) string { return ageColor(s.Last, age(s.Last)) },
		func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool { return a.Last.Before(b.Last) }},
	{"ctx", "CTX", 4, true,
		// Context pressure predicts compaction, so it gets the color scale. The
		// scale only applies to real percentages: with the model's window unknown
		// the cell is a raw token count, which must not hit the red path.
		func(s session.Session, db models.DB, _ RowMeta, _, _ int) string {
			cell := ctxPct(s, db)
			if !strings.HasSuffix(cell, "%") {
				if cell == "-" {
					return cell
				}
				return ansi("2", cell) // raw count, window unknown
			}
			switch f := ctxFrac(s, db); {
			case f >= 0.85:
				return ansi("1;31", cell)
			case f >= 0.6:
				return ansi("33", cell)
			}
			return cell
		},
		func(a, b session.Session, db models.DB, _ map[string]RowMeta) bool {
			return ctxFrac(a, db) < ctxFrac(b, db)
		}},
	{"cost", "COST", 8, true,
		// Money heat: the $900 session should not render like the $0.05 one.
		func(s session.Session, db models.DB, _ RowMeta, _, _ int) string {
			cell := costStr(s, db)
			switch v := costVal(s, db); {
			case v >= 100:
				return ansi("1;33", cell)
			case v >= 10:
				return ansi("33", cell)
			case v <= 0:
				return ansi("2", cell)
			}
			return cell
		},
		func(a, b session.Session, db models.DB, _ map[string]RowMeta) bool {
			return costVal(a, db) < costVal(b, db)
		}},
	{"tokens", "TOKENS", 9, true,
		func(s session.Session, _ models.DB, _ RowMeta, _, _ int) string {
			if s.InTok == 0 && s.OutTok == 0 {
				return ansi("2", "-")
			}
			return ansi("2", humanTok(s.InTok)) + "/" + humanTok(s.OutTok)
		},
		func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool {
			return (a.InTok + a.OutTok) < (b.InTok + b.OutTok)
		}},
	{"dir", "DIR", 30, false,
		func(s session.Session, _ models.DB, m RowMeta, w, _ int) string {
			if m.DirGone {
				// folder renamed/moved away: mark it (resume prompts to relink).
				return ansi("1;33", "!"+tildeTail(s.Dir, w-1))
			}
			return dirCell(s.Dir, w)
		},
		func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool { return a.Dir < b.Dir }},
	{"win", "WIN", 14, false,
		// The jumpable tmux target ("session:window"), not the raw locator: the
		// pane suffix is dropped and the ":window" index is preserved on overflow,
		// so the row names the same window tmux does (see winTarget).
		func(_ session.Session, _ models.DB, m RowMeta, w, _ int) string {
			return ansi("36", winTarget(m.Locator, w))
		},
		func(a, b session.Session, _ models.DB, meta map[string]RowMeta) bool {
			return meta[session.Key(a)].Locator < meta[session.Key(b)].Locator
		}},
	{"title", "TITLE", 80, false,
		func(s session.Session, _ models.DB, _ RowMeta, _, _ int) string {
			if time.Since(s.Last) > 7*24*time.Hour {
				return ansi("2", title(s)) // stale rows recede as a whole
			}
			return title(s)
		},
		func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool { return a.Title < b.Title }},
	// id is off by default (not in defaultOrder) but available via config
	// columns: ["id", ...]. Shows the first 8 chars of the session id so a user
	// can visually identify a session or confirm a filter landed on the right one.
	{"id", "ID", 8, false,
		func(s session.Session, _ models.DB, _ RowMeta, _, _ int) string {
			if len(s.ID) > 8 {
				return ansi("2", s.ID[:8])
			}
			return ansi("2", s.ID)
		},
		func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool { return a.ID < b.ID }},
}

// colHelp is a one-line human description per registry column key, shown in the
// picker's column-management modal so a user knows what each column carries. A
// tag:<key> column is described dynamically (see AllColumns).
var colHelp = map[string]string{
	"host":      "machine the session runs on (blank for local)",
	"status":    "merged status: needs-you / working / crashed",
	"lifecycle": "display lifecycle: working-live / waiting-live / done-resident / concluded",
	"archived":  "hidden from default views, reversible",
	"harness":   "the agent tool (claude, codex, ...)",
	"state":     "live / idle / crashed marker",
	"spin":      "activity spinner",
	"activity":  "what the session is doing right now",
	"yolo":      "⚠ when running without guardrails",
	"name":      "session display name",
	"run":       "the run (group) it belongs to",
	"tags":      "labels attached to the session",
	"model":     "the model in use",
	"effort":    "reasoning effort level",
	"age":       "time since last activity",
	"ctx":       "context-window pressure",
	"cost":      "dollars spent so far",
	"tokens":    "tokens in / out",
	"dir":       "working directory",
	"win":       "tmux window target",
	"title":     "latest turn summary",
	"id":        "session id (first 8 chars)",
}

// ColInfo is one column's static identity for the column-management modal: its
// stable key, header label, human description, built-in default width, and
// whether it is part of the built-in default layout.
type ColInfo struct {
	Key            string
	Label          string
	Help           string
	Width          int
	DefaultVisible bool
}

// colDisplayLabel is a column's header label, falling back to the uppercased key
// for the two glyph-only columns (spin, yolo) whose label is blank, so the modal
// can name every column.
func colDisplayLabel(c column) string {
	if c.label != "" {
		return c.label
	}
	return strings.ToUpper(c.key)
}

// AllColumns is every column the modal can list: the full registry in registry
// order, followed by any tag:<key> columns pinned in the given layout. Built-in
// visibility is membership in the default layout; tag columns are treated visible
// (they are only present because the user pinned them).
func AllColumns(cfg config.Config) []ColInfo {
	inDefault := map[string]bool{}
	for _, k := range defaultOrder {
		inDefault[k] = true
	}
	out := make([]ColInfo, 0, len(registry)+2)
	for _, c := range registry {
		out = append(out, ColInfo{
			Key: c.key, Label: colDisplayLabel(c), Help: colHelp[c.key],
			Width: c.width, DefaultVisible: inDefault[c.key],
		})
	}
	seen := map[string]bool{}
	for _, k := range cfg.Columns {
		kk := colKeyAlias(strings.ToLower(strings.TrimSpace(k)))
		if key, ok := strings.CutPrefix(kk, "tag:"); ok && key != "" && !seen[kk] {
			seen[kk] = true
			tc := tagColumn(key)
			out = append(out, ColInfo{
				Key: tc.key, Label: tc.label, Help: "value of the " + key + " label",
				Width: tc.width, DefaultVisible: true,
			})
		}
	}
	return out
}

// ColKey normalizes a config/saved column token to its stable registry key
// (lowercased, trimmed, with the legacy "group"->"run" alias resolved). Exported
// for the picker's column modal, which matches saved and config keys against the
// registry the same way layout() does.
func ColKey(s string) string { return colKeyAlias(strings.ToLower(strings.TrimSpace(s))) }

// statusCell renders the merged status: attention first, then activity (with
// the spinner riding the working state), then crash, else blank (inactive).
func statusCell(m RowMeta, frame int) string {
	switch m.Waiting {
	case "done":
		return ansi("1;32", "✓ review") // tagged success, waiting on you to accept the result
	case "input":
		return ansi("1;31", "needs you")
	case "auth":
		return ansi("1;33", "needs auth")
	case "children":
		return ansi("1;36", "waiting")
	}
	if m.Failed { // a headless run errored: surfaced distinctly, not a frozen working/idle
		return ansi("1;31", "✗ failed")
	}
	if m.Done { // a task-carrying worker concluded: halted, not a frozen idle
		return ansi("1;36", "✓ done")
	}
	switch {
	case m.Activity == Working:
		return ansi("1;32", spinner[frame%len(spinner)]+" working")
	case m.Detached && m.State == StateLive:
		return ansi("2", "detached") // running in the background, no window here
	case m.State == StateLive:
		return ansi("2", "idle")
	case m.State == StateCrash:
		return ansi("1;33", "crash")
	}
	return ""
}

// statusRank orders the merged status column by urgency.
func statusRank(m RowMeta) int {
	switch {
	case m.Waiting != "":
		return 0
	case m.Failed:
		return 1
	case m.Done:
		return 2
	case m.Activity == Working:
		return 3
	case m.State == StateLive:
		return 4
	case m.State == StateCrash:
		return 5
	}
	return 6
}

func stateCell(state string) string {
	switch state {
	case StateLive:
		return ansi("1;32", StateLive)
	case StateCrash:
		return ansi("1;33", StateCrash)
	}
	return ""
}

func stateRank(state string) int {
	switch state {
	case StateLive:
		return 0
	case StateCrash:
		return 1
	}
	return 2
}

func lifecycleCell(life string) string {
	switch life {
	case state.LifecycleLive:
		return ansi("1;32", life)
	case state.DisplayLiveWorking:
		return ansi("1;32", "working-live")
	case state.DisplayLiveWaiting:
		return ansi("1;36", "waiting-live")
	case state.DisplayLiveDoneResident:
		return ansi("1;36", "done-resident")
	case state.LifecycleConcluded:
		return ansi("1;36", life)
	case state.LifecycleCrashed:
		return ansi("1;33", life)
	case state.LifecycleDormant:
		return ansi("2", life)
	}
	return ""
}

func displayLife(m RowMeta) string {
	if m.DisplayPhase != "" {
		return m.DisplayPhase
	}
	return m.Lifecycle
}

func lifecycleRank(life string) int {
	switch life {
	case state.LifecycleLive:
		return 0
	case state.DisplayLiveWorking:
		return 0
	case state.DisplayLiveWaiting:
		return 1
	case state.DisplayLiveDoneResident:
		return 2
	case state.LifecycleCrashed:
		return 3
	case state.LifecycleConcluded:
		return 4
	case state.LifecycleDormant:
		return 5
	}
	return 6
}

// spinCell is the standalone animated activity light: it spins only while a
// session is actively working, and is blank otherwise.
func spinCell(a string, frame int) string {
	if a == Working {
		return ansi("1;32", spinner[frame%len(spinner)])
	}
	return ""
}

// activityCell is the ACT label. A session blocked on a human is surfaced
// distinctly ("needs you" for an input/ask prompt, "needs auth" for an OAuth
// flow), overriding working/idle so attention is unmissable.
func activityCell(m RowMeta) string {
	switch m.Waiting {
	case "done":
		return ansi("1;32", "✓ review")
	case "input":
		return ansi("1;31", "needs you")
	case "auth":
		return ansi("1;33", "needs auth")
	case "children":
		return ansi("1;36", "waiting")
	}
	if m.Failed {
		return ansi("1;31", "failed")
	}
	if m.Done {
		return ansi("1;36", "done")
	}
	switch m.Activity {
	case Working:
		return ansi("1;32", Working)
	case Idle:
		if m.Detached {
			return ansi("2", "detached")
		}
		return ansi("2", Idle)
	}
	return ""
}

// FooterState renders the preview footer's state label: the same priority
// order as statusCell/activityCell (attention beats a concluded task beats
// working beats detached/idle/crash), so the three readings never disagree.
// Only the working case reports an animated spinner; every other state -
// including a future terminal marker like a failed task, which slots in next
// to the Done check above - is frozen, so the spinner is unambiguous evidence
// of a live, active process.
func FooterState(m RowMeta, frame int) string {
	switch m.Waiting {
	case "done":
		return ansi("1;32", "✓ review ready")
	case "input":
		return ansi("1;31", "needs input")
	case "auth":
		return ansi("1;33", "needs auth")
	case "children":
		return ansi("1;36", "waiting")
	}
	if m.Done {
		return ansi("1;36", "done")
	}
	switch {
	case m.Activity == Working:
		return ansi("1;32", spinner[frame%len(spinner)]+" working")
	case m.Detached && m.State == StateLive:
		return ansi("2", "detached")
	case m.State == StateLive:
		return ansi("2", "idle")
	case m.State == StateCrash:
		return ansi("1;31", "crashed")
	}
	return ansi("2", "inactive")
}

// FooterLine is the full preview-footer status: the state label plus how long
// since the transcript itself last changed. A hook-reported "working" can hold
// through a long silent reasoning turn (see state.HookFresh), so the age half
// is what tells a silent-but-alive turn apart from a stall: both read
// "working", but only the stall's age keeps climbing.
func FooterLine(m RowMeta, lastOutput time.Time, frame int) string {
	return FooterState(m, frame) + ansi("2", "  ·  "+lastOutputAge(lastOutput))
}

func lastOutputAge(t time.Time) string {
	if t.IsZero() {
		return "no output yet"
	}
	a := age(t)
	if a == "now" {
		return "last output just now"
	}
	return "last output " + a + " ago"
}

// displayName is a session's name in the picker: its control-layer name, else its
// transcript title.
func displayName(s session.Session) string {
	if s.Name != "" {
		return s.Name
	}
	return title(s)
}

func activityRank(a string) int {
	switch a {
	case Working:
		return 0
	case Idle:
		return 1
	}
	return 2
}

// lifecycle is deliberately NOT in the default layout: STATUS already renders
// the attention/activity/terminal facts, and two overlapping state vocabularies
// side by side ("needs you" next to "waiting-live") read as incoherence. The
// LIFE column stays available via config columns = [..., "lifecycle", ...] and
// the wire format still carries the canonical lifecycle word.
var defaultOrder = []string{"harness", "status", "model", "age", "ctx", "cost", "tokens", "dir", "win", "title"}

const defaultSortKey = "age"

// colKeyAlias normalizes a deprecated column key to its current name, so a
// config written before the group->run rename ("group", the old name for the
// RUN column) still resolves.
func colKeyAlias(key string) string {
	if key == "group" {
		return "run"
	}
	return key
}

// hasCol reports whether the layout lists a column key (case-insensitive).
// "tag:" matches any tag column ("tags" or a pinned "tag:<key>").
func hasCol(keys []string, name string) bool {
	name = colKeyAlias(name)
	for _, k := range keys {
		kk := colKeyAlias(strings.ToLower(strings.TrimSpace(k)))
		if kk == name || (name == "tag:" && (kk == "tags" || strings.HasPrefix(kk, "tag:"))) {
			return true
		}
	}
	return false
}

// insertCols returns cfg with names inserted after the first anchor present in
// the layout, or appended before "title" (else at the end) when no anchor
// matches, so an auto-column still appears for a user whose custom column list
// omits the usual anchors. Names already listed (per hasCol) are skipped. This
// is the one mechanism behind every auto-inserted column.
func insertCols(cfg config.Config, anchors []string, names ...string) config.Config {
	keys := cfg.Columns
	if len(keys) == 0 {
		keys = append([]string{}, defaultOrder...)
	}
	var missing []string
	for _, n := range names {
		probe := n
		if n == "tags" {
			probe = "tag:" // a pinned tag:<key> column already covers tags
		}
		if !hasCol(keys, probe) {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		cfg.Columns = keys
		return cfg
	}
	anchor := ""
	for _, a := range anchors {
		if hasCol(keys, a) {
			anchor = a
			break
		}
	}
	var out []string
	for _, k := range keys {
		kk := strings.ToLower(strings.TrimSpace(k))
		if anchor == "" && kk == "title" {
			out = append(out, missing...) // no anchor: land before the wide title
			missing = nil
		}
		out = append(out, k)
		if kk == anchor {
			out = append(out, missing...)
			missing = nil
		}
	}
	out = append(out, missing...) // no anchor and no title: at the end
	cfg.Columns = out
	return cfg
}

// WithHostColumn ensures the HOST column at the front of the layout, so the
// picker shows which machine a session lives on only when federating.
func WithHostColumn(cfg config.Config) config.Config {
	keys := cfg.Columns
	if len(keys) == 0 {
		keys = defaultOrder
	}
	if hasCol(keys, "host") {
		return cfg
	}
	cfg.Columns = append([]string{"host"}, keys...)
	return cfg
}

// WithLifecycleColumn ensures the canonical lifecycle class is visible in
// session lists, even when a custom column set predates the field.
func WithLifecycleColumn(cfg config.Config) config.Config {
	return insertCols(cfg, []string{"status", "activity", "state", "harness"}, "lifecycle")
}

// WithGroupColumns inserts NAME and ID when any session belongs to a run, so
// worker names and the actual session handle are visible without a config edit.
func WithGroupColumns(cfg config.Config) config.Config {
	return insertCols(cfg, []string{"status", "activity", "state", "harness"}, "name", "id")
}

// WithTagsColumn inserts TAGS when any session carries labels. A pinned
// tag:<key> column counts as already showing tags.
func WithTagsColumn(cfg config.Config) config.Config {
	return insertCols(cfg, []string{"run", "status", "activity", "state", "harness"}, "tags")
}

// WithYoloColumn inserts the ⚠ column when any live session runs without
// guardrails, so an unsupervised agent is unmissable.
func WithYoloColumn(cfg config.Config) config.Config {
	return insertCols(cfg, []string{"status", "activity", "state", "harness"}, "yolo")
}

// WithEffortColumn inserts EFF only when a session actually carries a
// reasoning-effort setting; an all-blank column is dead width for everyone else.
func WithEffortColumn(cfg config.Config) config.Config {
	return insertCols(cfg, []string{"model"}, "effort")
}

// layoutCache memoizes the resolved column layout per column list: Row runs for
// every visible row every animation frame, and rebuilding the registry map each
// call was measurable garbage for zero change (the list is fixed per process).
var layoutCache sync.Map // joined column keys -> []column

func layout(cfg config.Config) []column {
	keys := cfg.Columns
	if len(keys) == 0 {
		keys = defaultOrder
	}
	cacheKey := strings.Join(keys, ",")
	if len(cfg.ColWidths) > 0 {
		// Widths are a runtime override (the column modal), so the cache must key on
		// them too or a resize would return the pre-resize layout.
		var b strings.Builder
		b.WriteString("|w:")
		for _, k := range keys {
			if w, ok := cfg.ColWidths[colKeyAlias(strings.ToLower(strings.TrimSpace(k)))]; ok && w > 0 {
				fmt.Fprintf(&b, "%s=%d,", k, w)
			}
		}
		cacheKey += b.String()
	}
	if v, ok := layoutCache.Load(cacheKey); ok {
		return v.([]column)
	}
	byKey := map[string]column{}
	for _, c := range registry {
		byKey[c.key] = c
	}
	var out []column
	for _, k := range keys {
		kk := colKeyAlias(strings.ToLower(strings.TrimSpace(k)))
		if key, ok := strings.CutPrefix(kk, "tag:"); ok && key != "" {
			out = append(out, tagColumn(key)) // a per-key column, e.g. tag:workstream
			continue
		}
		if c, ok := byKey[kk]; ok {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		for _, k := range defaultOrder {
			out = append(out, byKey[k])
		}
	}
	if len(cfg.ColWidths) > 0 {
		for i := range out {
			if w, ok := cfg.ColWidths[out[i].key]; ok && w > 0 {
				out[i].width = w // resize from the column modal
			}
		}
	}
	layoutCache.Store(cacheKey, out)
	return out
}

// tagColumn is a per-key tag column: `tag:workstream` shows each session's value
// for the "workstream" key=value label, headed WORKSTREAM, sortable by value.
func tagColumn(key string) column {
	return column{
		key:   "tag:" + key,
		label: strings.ToUpper(key),
		width: 14,
		cell: func(s session.Session, _ models.DB, _ RowMeta, w, _ int) string {
			return clip(session.LabelValue(s.Labels, key), w)
		},
		less: func(a, b session.Session, _ models.DB, _ map[string]RowMeta) bool {
			return session.LabelValue(a.Labels, key) < session.LabelValue(b.Labels, key)
		},
	}
}

// NumCols is the number of visible columns (the H/L sort cursor range).
func NumCols(cfg config.Config) int { return len(layout(cfg)) }

// DefaultSortCol is the index of the default sort column in the layout.
func DefaultSortCol(cfg config.Config) int {
	for i, c := range layout(cfg) {
		if c.key == defaultSortKey {
			return i
		}
	}
	return 0
}

// DefaultDescFor is the direction a column sorts on its first selection.
func DefaultDescFor(cfg config.Config, col int) bool {
	cols := layout(cfg)
	if col >= 0 && col < len(cols) {
		return cols[col].desc
	}
	return false
}

// ColumnKey is the stable key of a visible layout column.
func ColumnKey(cfg config.Config, col int) string {
	cols := layout(cfg)
	if col >= 0 && col < len(cols) {
		return cols[col].key
	}
	return ""
}

// ColumnIndex is the visible layout index for a stable column key.
func ColumnIndex(cfg config.Config, key string) int {
	key = ColKey(key)
	for i, c := range layout(cfg) {
		if c.key == key {
			return i
		}
	}
	return -1
}

// ColumnLabel is the display label of a visible layout column.
func ColumnLabel(cfg config.Config, col int) string {
	cols := layout(cfg)
	if col >= 0 && col < len(cols) {
		return colDisplayLabel(cols[col])
	}
	return ""
}

// ColumnFilterText is the untruncated, plain-text value used by the picker's
// column-scoped insert filter. It follows each column's user-facing value rather
// than the padded/rendered row, so filtering does not miss clipped cells.
func ColumnFilterText(cfg config.Config, db models.DB, s session.Session, m RowMeta, col int) string {
	cols := layout(cfg)
	if col < 0 || col >= len(cols) {
		return ""
	}
	c := cols[col]
	switch c.key {
	case "host":
		return hostLabel(s.Host)
	case "status":
		return StripANSI(statusCell(m, 0))
	case "lifecycle":
		return displayLife(m)
	case "archived":
		if m.Archived || s.Archived {
			return "yes"
		}
		return ""
	case "harness":
		return s.Harness
	case "state":
		return m.State
	case "spin":
		return m.Activity
	case "activity":
		return StripANSI(activityCell(m))
	case "yolo":
		if m.Yolo {
			return "yolo unsandboxed"
		}
		return ""
	case "name":
		return displayName(s)
	case "run":
		return s.Group
	case "tags":
		return strings.Join(s.Labels, " ")
	case "model":
		return shortModel(s.Model)
	case "effort":
		return s.Effort
	case "age":
		return age(s.Last)
	case "ctx":
		return ctxPct(s, db)
	case "cost":
		return costStr(s, db)
	case "tokens":
		if s.InTok == 0 && s.OutTok == 0 {
			return "-"
		}
		return fmt.Sprintf("%s/%s %d/%d", humanTok(s.InTok), humanTok(s.OutTok), s.InTok, s.OutTok)
	case "dir":
		return strings.TrimSpace(TildePath(s.Dir) + " " + s.Dir)
	case "win":
		return winTarget(m.Locator, 0)
	case "title":
		if s.Title != "" {
			return s.Title
		}
		return "(no title)"
	case "id":
		return s.ID
	default:
		if key, ok := strings.CutPrefix(c.key, "tag:"); ok {
			return session.LabelValue(s.Labels, key)
		}
		return StripANSI(c.cell(s, db, m, c.width, 0))
	}
}

// Row renders one session's visible columns (with the spinner at frame). Every
// column except the last is padded to its width; the last runs to the edge.
func Row(cfg config.Config, db models.DB, s session.Session, m RowMeta, frame int) string {
	cols := layout(cfg)
	parts := make([]string, len(cols))
	for i, c := range cols {
		v := c.cell(s, db, m, c.width, frame)
		if i < len(cols)-1 {
			v = pad(v, c.width)
		}
		parts[i] = v
	}
	row := strings.Join(parts, " ")
	if m.Archived || m.State != StateLive {
		row = dimRow(row) // non-live (crash, done, inactive) rows recede
	}
	return row
}

// dimRow fades a whole row to the faint intensity so a non-live session recedes
// behind the live ones, without touching column widths (the added SGR codes are
// zero-width and StripANSI-invisible). It re-applies the dim after every cell's
// own reset, exactly as tintRow re-applies its background, since each cell ends
// in \x1b[0m which would otherwise cancel the fade. A cell that sets its own
// color reasserts it, so the per-state hues in the STATUS/STATE cells stay
// readable while the plain bulk of the row greys out.
func dimRow(s string) string {
	const dim = "\x1b[2m"
	return dim + strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+dim) + "\x1b[0m"
}

// Columns is the header row. The sorted column gets a ▲/▼ arrow; the selected
// (H/L) column is reverse-highlighted. Widths match Row.
func Columns(cfg config.Config, selected, sortCol int, desc bool) string {
	cols := layout(cfg)
	arrow := "▲"
	if desc {
		arrow = "▼"
	}
	parts := make([]string, len(cols))
	for i, c := range cols {
		lab := c.label
		if i == sortCol {
			lab += arrow
		}
		if i < len(cols)-1 {
			lab = pad(lab, c.width)
		}
		if i == selected {
			lab = "\x1b[7m" + lab + "\x1b[0m"
		}
		parts[i] = lab
	}
	return strings.Join(parts, " ")
}

// Sort orders sessions by a layout column (ascending base; desc reverses).
func Sort(cfg config.Config, ss []session.Session, db models.DB, col int, desc bool, meta map[string]RowMeta) {
	cols := layout(cfg)
	if col < 0 || col >= len(cols) {
		return
	}
	less := cols[col].less
	sort.SliceStable(ss, func(i, j int) bool {
		if desc {
			return less(ss[j], ss[i], db, meta)
		}
		return less(ss[i], ss[j], db, meta)
	})
}

// Metrics are the navbar summary over the filtered rows.
type Metrics struct {
	Sessions int
	Live     int
	Cost     float64
}

func (m Metrics) String() string {
	return fmt.Sprintf("%d sessions · %s · %d live", m.Sessions, costShort(m.Cost), m.Live)
}

// Host federation states, shown in the machines roster.
const (
	HostLocal   = "local"   // this machine
	HostOnline  = "online"  // answered ax list --json
	HostOffline = "offline" // transport failed (unreachable or timed out)
	HostNoAx    = "no ax"   // reachable, but ax is not installed there
	HostStale   = "old ax"  // answered with an incompatible version
	HostPending = "pending" // fetch still in flight; flips to a real state when it answers
)

// HostStatus is one machine in the roster: its name, federation state, and how
// many sessions it contributed.
type HostStatus struct {
	Name     string
	State    string // one of the Host* values
	Sessions int
}

// NetHostStatus is one host's row in the network/health view: its reachability
// and latency, ax/wire version with a compat marker, OS/shell, harness set, and
// profile in-sync/drift verdict. It is the single shape shared by `ax config
// status` (its --json envelope, hence the json tags) and the picker's network
// panel, so both read one code path rather than two. Fail-open: an offline /
// no-ax / version-mismatched host is a row in that state, never a gap.
type NetHostStatus struct {
	Host        string   `json:"host"`
	State       string   `json:"state"` // HostOnline / HostOffline / HostNoAx
	Reachable   bool     `json:"reachable"`
	LatencyMS   int64    `json:"latency_ms,omitempty"`
	AxVersion   string   `json:"ax_version,omitempty"`
	WireVersion int      `json:"wire_version,omitempty"`
	Compat      string   `json:"compat"` // ok / newer / older / unknown
	OS          string   `json:"os,omitempty"`
	Shell       string   `json:"shell,omitempty"`
	Harnesses   []string `json:"harnesses,omitempty"`
	Sync        string   `json:"sync"` // in-sync / drift / unknown / unreachable
}

// MachineTag renders the active machine filter: a marker plus the machine's name
// colored by status (and a session count or a reason). Shown in the prompt line
// as you rotate the filter with the machines key.
func MachineTag(h HostStatus) string {
	return ansi("1;36", "▸") + " " + hostSeg(h)
}

func hostSeg(h HostStatus) string {
	switch h.State {
	case HostOnline, HostLocal:
		return ansi("1;32", h.Name) + " " + ansi("2", fmt.Sprintf("%d", h.Sessions))
	case HostOffline:
		return ansi("1;31", h.Name+" offline")
	case HostNoAx:
		return ansi("1;33", h.Name+" no ax")
	case HostStale:
		return ansi("1;33", h.Name+" old ax")
	case HostPending:
		return ansi("2", h.Name+" …")
	}
	return h.Name
}

// Age renders a timestamp as the compact age the AGE column uses ("6m", "2h").
func Age(t time.Time) string { return age(t) }

// CostShort renders a dollar amount compactly ("$0.32", "$12", "$1.2k").
func CostShort(c float64) string { return costShort(c) }

// HumanTok renders a token count compactly ("420", "12k", "3.4M").
func HumanTok(n int) string { return humanTok(n) }

func costShort(c float64) string {
	switch {
	case c >= 1000:
		return fmt.Sprintf("$%.1fk", c/1000)
	case c >= 1:
		return fmt.Sprintf("$%.0f", c)
	default:
		return fmt.Sprintf("$%.2f", c)
	}
}

// Navbar is the top bar: dim version + mode badge on the left, then all status
// flexed right (filter/mode segment, scope tag, metrics, help), joined by dashes.
// state is the pre-composed filter/mode segment (machine, group, tree), already
// colored; scope names the active scope filter ("" for all) and prefixes the
// metrics with a tag. The bottom row holds only key hints, so every piece of
// state shows here.
func Navbar(mode string, m Metrics, scope string, state string, width int) string {
	w := width - 2
	if w < 30 {
		w = 30
	}
	left := " ax " + build.Display() + " "
	badge := " " + mode + " "
	tag := ""
	if scope != "" {
		tag = scope + " · "
	}
	mseg := "" // the filter/mode status segment (already colored), when non-empty
	if state != "" {
		mseg = state + "  "
	}
	metrics := m.String() + "  ? help "
	// width from the plain text; machine may carry ANSI, so strip it for counting
	rightPlain := " " + StripANSI(mseg) + tag + metrics
	rem := w - utf8.RuneCountInString(left) - utf8.RuneCountInString(badge) - utf8.RuneCountInString(rightPlain)
	if rem < 2 {
		rem = 2
	}
	rightRendered := " " + mseg
	if scope != "" {
		rightRendered += ansi("1;33", tag)
	}
	rightRendered += ansi("2", metrics)
	return ansi("2", left) + modeBadge(mode, badge) + strings.Repeat("─", rem) + rightRendered
}

func modeBadge(mode, badge string) string {
	color := "1;36" // NORMAL: bold cyan
	switch mode {
	case "VISUAL":
		color = "1;35" // bold magenta, vim's visual color
	case "INSERT":
		color = "1;32" // bold green
	}
	return ansi(color, badge)
}

// TextFile is the file content search greps for a session: the clean text-cache
// sidecar when the harness has one, else the transcript itself. Remote sessions
// have no local transcript, so they return "" and are excluded from search.
func TextFile(cfg config.Config, s session.Session) string {
	if s.Host != "" {
		return ""
	}
	for _, h := range cfg.Harnesses {
		if h.Name == s.Harness {
			if h.Format == "opencode" {
				return s.File
			}
			return textcache.Ensure(h.Format, s.File)
		}
	}
	return s.File
}

// Preview renders a session's metadata header plus its recent turns.
func Preview(cfg config.Config, db models.DB, s session.Session) string {
	if s.Mode == "recipe" {
		return recipePreview(db, s)
	}
	var format string
	for _, h := range cfg.Harnesses {
		if h.Name == s.Harness {
			format = h.Format
		}
	}
	if format == "" {
		return ""
	}
	// A remote session's transcript lives on its owning host. The picker fetches
	// the body on demand over the transport (see RemotePreview); this placeholder
	// is only the fallback shown when no transport is wired.
	if s.Host != "" {
		return previewHeader(s, db) + "\n" + strings.Repeat("─", 60) +
			"\n\nremote session on " + s.Host
	}
	if format == "opencode" {
		data, _ := os.ReadFile(s.File)
		return string(data)
	}
	var b strings.Builder
	b.WriteString(previewHeader(s, db) + "\n")
	b.WriteString(strings.Repeat("─", 60) + "\n")
	for _, t := range session.RecentText(format, s.File, 12) {
		who := "you"
		if t.Role == "assistant" {
			who = "agent"
		}
		fmt.Fprintf(&b, "\n[%s %s] %s\n", who, TurnTime(t.Time), clipLines(t.Text, 6))
	}
	return b.String()
}

func recipePreview(db models.DB, s session.Session) string {
	var b strings.Builder
	b.WriteString(previewHeader(s, db) + "\n")
	if s.RecipePath != "" {
		b.WriteString("recipe: " + s.RecipePath + "\n")
	}
	if len(s.RecipeInterpreter) > 0 {
		b.WriteString("interpreter: " + strings.Join(s.RecipeInterpreter, " ") + "\n")
	}
	b.WriteString(strings.Repeat("─", 60) + "\n")
	lines := tailLines(s.LogPath, 80)
	if len(lines) == 0 {
		b.WriteString("\nno recipe output yet")
		return b.String()
	}
	b.WriteString("\n")
	b.WriteString(strings.Join(lines, "\n"))
	return b.String()
}

func tailLines(path string, n int) []string {
	if path == "" || n <= 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	text := strings.TrimRight(string(data), "\r\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// Highlight wraps every case-insensitive occurrence of query in reverse video,
// for content-mode preview.
func Highlight(text, query string) string {
	if query == "" {
		return text
	}
	lt, lq := strings.ToLower(text), strings.ToLower(query)
	var b strings.Builder
	for i := 0; ; {
		j := strings.Index(lt[i:], lq)
		if j < 0 {
			b.WriteString(text[i:])
			return b.String()
		}
		j += i
		b.WriteString(text[i:j])
		b.WriteString("\x1b[7m")
		b.WriteString(text[j : j+len(query)])
		b.WriteString("\x1b[0m")
		i = j + len(query)
	}
}

func previewHeader(s session.Session, db models.DB) string {
	last := "last " + age(s.Last) + " ago"
	if age(s.Last) == "now" {
		last = "active now" // "last now ago" reads like a typo
	}
	// Identity leads: what you are about to step into should be unambiguous
	// before any stats (name, then harness/model, then tags).
	who := s.Harness
	if s.Model != "" {
		who += " · " + s.Model
	}
	if s.Name != "" {
		who = ansi("1;36", s.Name) + " · " + who
	}
	if len(s.Labels) > 0 {
		// The full label set, never truncated: this is where a clipped TAGS cell
		// resolves to the whole story.
		parts := make([]string, len(s.Labels))
		for i, l := range s.Labels {
			parts[i] = colorLabel(l)
		}
		who += "\ntags: " + strings.Join(parts, " ")
	}
	h := fmt.Sprintf("%s\n%s\n%s · in %s out %s · ctx %s · %s",
		who, s.Dir, last,
		humanTok(s.InTok), humanTok(s.OutTok), ctxPct(s, db), costStr(s, db))
	// Control-layer facts, when this session is part of a run.
	if s.Group != "" {
		line := "run " + s.Group
		if s.Origin != "" {
			line += " · " + s.Origin
		}
		if s.Mode != "" {
			line += " · " + s.Mode
		}
		if s.Outcome != "" {
			line += " · " + s.Outcome
		}
		h += "\n" + ansi("1;36", line)
		if s.Task != "" {
			h += "\ntask: " + clip(s.Task, 200)
		}
		if s.FailReason != "" {
			h += "\n" + ansi("1;31", "reason: "+clip(s.FailReason, 200))
		}
	}
	if q, ok := ask.Load(s.ID); ok && !q.Answered { // a pending ask, waiting on a human
		h += "\n" + ansi("1;31", "waiting: "+clip(q.Question, 200))
	}
	return h
}

// ---- numeric sort keys ----

func ctxFrac(s session.Session, db models.DB) float64 {
	if s.CtxTok == 0 {
		return -1
	}
	win := s.CtxWindow
	if win == 0 {
		if info, ok := db.Lookup(s.Model); ok {
			win = info.Context
		}
	}
	if win > 0 {
		return float64(s.CtxTok) / float64(win)
	}
	return float64(s.CtxTok)
}

func costVal(s session.Session, db models.DB) float64 {
	if s.HasCost {
		return s.Cost
	}
	info, ok := db.Lookup(s.Model)
	if !ok {
		return -1
	}
	return (float64(s.InTok)*info.Input + float64(s.OutTok)*info.Output +
		float64(s.CacheReadT)*info.CacheRead + float64(s.CacheWriteT)*info.CacheWrite) / 1e6
}

// Cost is the numeric cost of a session, for the navbar total.
func Cost(s session.Session, db models.DB) float64 { return costVal(s, db) }

// WindowTitle names the multiplexer window a session opens in. Remote sessions
// are prefixed with their host so the tmux status bar shows where they run. A
// session that belongs to a run is prefixed with its group so the whole family
// clusters together in the window list; the mux backend itself folds in its
// own ax namespace prefix ahead of this (see internal/mux), so the two never
// collide. This is purely the mux-level name: the picker's own title column
// reads session.Session.Title/Name instead and is untouched by any of this.
func WindowTitle(s session.Session) string {
	dir := strings.TrimRight(s.Dir, "/")
	if i := strings.LastIndexByte(dir, '/'); i >= 0 {
		dir = dir[i+1:]
	}
	title := dir + "·" + s.Harness
	if s.Host != "" {
		title = s.Host + ":" + title
	}
	if s.Group != "" {
		title = s.Group + "/" + title
	}
	return title
}

// ---- formatters ----

func title(s session.Session) string {
	if s.Title != "" {
		return clip(s.Title, 80)
	}
	return "(no title)"
}

func shortModel(m string) string {
	if m == "" {
		return "?"
	}
	return strings.TrimPrefix(m, "claude-")
}

// hostLabel renders the federation host, showing local sessions as "local" so
// the column reads cleanly next to remote host names.
func hostLabel(h string) string {
	if h == "" {
		return "local"
	}
	return h
}

func ctxPct(s session.Session, db models.DB) string {
	if s.CtxTok == 0 {
		return "-"
	}
	win := s.CtxWindow
	if win == 0 {
		if info, ok := db.Lookup(s.Model); ok {
			win = info.Context
		}
	}
	if win > 0 {
		return fmt.Sprintf("%d%%", s.CtxTok*100/win)
	}
	return humanTok(s.CtxTok)
}

func costStr(s session.Session, db models.DB) string {
	cost := s.Cost
	if !s.HasCost {
		info, ok := db.Lookup(s.Model)
		if !ok {
			// Model not in the price DB (e.g. newer than the snapshot): no
			// estimate is possible. A "?" next to the CTX cell's raw token
			// count read as one broken "152k ?" hybrid; "-" matches the
			// no-data convention the CTX cell already uses.
			return "-"
		}
		cost = (float64(s.InTok)*info.Input + float64(s.OutTok)*info.Output +
			float64(s.CacheReadT)*info.CacheRead + float64(s.CacheWriteT)*info.CacheWrite) / 1e6
	}
	switch {
	case cost == 0:
		return "$0"
	case cost < 0.01:
		return "<1¢"
	}
	return fmt.Sprintf("$%.2f", cost)
}

// TurnTime renders a transcript turn's timestamp as an absolute local
// "YYYY-MM-DD HH:MM". Sessions span days, so a bare clock time on a turn marker
// is ambiguous; the compact relative age lives in the list's AGE column instead.
// "?" when the transcript carried no parseable timestamp.
func TurnTime(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func age(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func humanTok(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// pad fits s to n display cells (CJK/emoji = 2, ANSI = 0), truncating or
// right-padding so columns align regardless of content.
func pad(s string, n int) string {
	w := dispWidth(s)
	switch {
	case w > n:
		return runewidth.Truncate(s, n, "")
	case w < n:
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// dispWidth is the display cell width, ignoring ANSI color escapes.
func dispWidth(s string) int { return runewidth.StringWidth(StripANSI(s)) }

// StripANSI removes SGR color escapes, leaving the visible text.
func StripANSI(s string) string {
	if !strings.ContainsRune(s, '\x1b') {
		return s
	}
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if runewidth.StringWidth(s) <= n {
		return s
	}
	return runewidth.Truncate(s, n-1, "") + "…"
}

func clipLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, "\n")
}

// TildePath renders a path with the home directory as ~ ("(no dir)" when
// empty). The one copy of the tilde rule; row cells and group headers share it.
func TildePath(p string) string {
	if p == "" {
		return "(no dir)"
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		p = "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// HostLabel renders the federation host, showing local sessions as "local" so
// the column reads cleanly next to remote host names.
func HostLabel(h string) string { return hostLabel(h) }

// Clip truncates s to n display cells with an ellipsis, collapsing whitespace.
// Rune-safe (byte slicing would cut CJK/emoji mid-rune).
func Clip(s string, n int) string { return clip(s, n) }

// dirCell renders a directory with the basename bright and the parent path dim,
// so a column of repeated long paths reads at a glance. The text is clipped
// plain first, then colored, so ANSI never lands inside a truncation.
func dirCell(dir string, w int) string {
	p := tildeTail(dir, w)
	i := strings.LastIndexByte(p, '/')
	if i < 0 || i == len(p)-1 {
		return p
	}
	return ansi("2", p[:i+1]) + p[i+1:]
}

// tagsCell renders labels with dim keys and hued values, never abbreviating:
// what fits renders whole, and the first label that does not fit is truncated
// with an ellipsis. The full set is always visible in the preview header and
// the label editor (l).
func tagsCell(labels []string, w int) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	for i, l := range labels {
		sep := 0
		if i > 0 {
			sep = 1
		}
		if lw := runewidth.StringWidth(l); used+sep+lw <= w {
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(colorLabel(l))
			used += sep + lw
			continue
		}
		room := w - used - sep - 1 // reserve the ellipsis cell
		if room < 1 {
			break
		}
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(colorLabelClipped(l, room) + ansi("2", "…"))
		break
	}
	return b.String()
}

// colorLabel renders one label: dim key, hued value (bare labels all hue).
func colorLabel(l string) string {
	if k, v, ok := strings.Cut(l, "="); ok {
		return ansi("2", k+"=") + ansi(hueFor(v), v)
	}
	return ansi(hueFor(l), l)
}

// colorLabelClipped is colorLabel truncated to room display cells.
func colorLabelClipped(l string, room int) string {
	if k, v, ok := strings.Cut(l, "="); ok {
		kw := runewidth.StringWidth(k) + 1
		if room <= kw {
			return ansi("2", runewidth.Truncate(k+"=", room, ""))
		}
		return ansi("2", k+"=") + ansi(hueFor(v), runewidth.Truncate(v, room-kw, ""))
	}
	return ansi(hueFor(l), runewidth.Truncate(l, room, ""))
}

func tildeTail(p string, n int) string {
	p = TildePath(p)
	if runewidth.StringWidth(p) <= n {
		return p
	}
	r := []rune(p)
	w, i := 0, len(r)
	for i > 0 {
		rw := runewidth.RuneWidth(r[i-1])
		if w+rw > n-1 {
			break
		}
		w += rw
		i--
	}
	return "…" + string(r[i:])
}

// winTarget renders a tmux locator ("session:window.pane") as the target the
// user types to jump to that window: "session:window". The pane suffix is
// dropped (ax opens one pane per window, so it never disambiguates), and when
// the target overflows the column the session name is trimmed from the left so
// the ":window" index (the switch key, and what tmux shows for the window)
// always survives. This is what makes a row and its real tmux window read as
// the same thing, instead of a dir-derived fragment with the index cut off.
func winTarget(loc string, w int) string {
	if loc == "" {
		return ""
	}
	// "session:window.pane" -> "session:window": the window is the switch unit.
	if c := strings.IndexByte(loc, ':'); c >= 0 {
		if d := strings.IndexByte(loc[c:], '.'); d >= 0 {
			loc = loc[:c+d]
		}
	}
	if w <= 0 || runewidth.StringWidth(loc) <= w {
		return loc
	}
	c := strings.IndexByte(loc, ':')
	if c < 0 { // no session half to trim (unexpected shape): plain right-clip
		return runewidth.Truncate(loc, w, "…")
	}
	tail := loc[c:] // ":window", the jump key we must keep
	if runewidth.StringWidth(tail)+1 >= w {
		return runewidth.Truncate(tail, w, "…") // no room for session chars
	}
	room := w - runewidth.StringWidth(tail) - 1 // 1 cell for the ellipsis
	return "…" + lastCells(loc[:c], room) + tail
}

// lastCells returns the last n display cells of s (rune-safe, no ellipsis), so
// a right-anchored trim keeps the distinguishing tail of a name.
func lastCells(s string, n int) string {
	r := []rune(s)
	w, i := 0, len(r)
	for i > 0 {
		rw := runewidth.RuneWidth(r[i-1])
		if w+rw > n {
			break
		}
		w += rw
		i--
	}
	return string(r[i:])
}

func ansi(code, s string) string { return "\x1b[" + code + "m" + s + "\x1b[0m" }
