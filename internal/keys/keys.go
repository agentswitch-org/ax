// Package keys is the picker's keymap: the set of rebindable actions, their
// default keys, and the resolution between a pressed key and the action it
// runs. Config supplies overrides (action -> key or list of keys); the picker
// dispatches through the resulting Map, and the help screen renders from it, so
// what the help shows always matches what the keys do.
//
// Enter (open), Esc (leave a mode), Ctrl-C (abort), and the arrow keys are
// structural and handled by the picker directly, so they are not listed here.
package keys

// Action is a picker action a key can be bound to.
type Action string

const (
	Down            Action = "down"
	Up              Action = "up"
	Top             Action = "top"
	Bottom          Action = "bottom"
	HalfDown        Action = "half_down"
	HalfUp          Action = "half_up"
	PreviewDown     Action = "preview_down"
	PreviewUp       Action = "preview_up"
	PreviewHalfDown Action = "preview_half_down"
	PreviewHalfUp   Action = "preview_half_up"
	PreviewTop      Action = "preview_top"
	PreviewBottom   Action = "preview_bottom"
	NextMatch       Action = "next_match"
	PrevMatch       Action = "prev_match"
	ColPrev         Action = "col_prev"
	ColNext         Action = "col_next"
	Sort            Action = "sort"
	ColExpand       Action = "col_expand"
	Scope           Action = "scope"
	Archive         Action = "archive"
	ToggleArchived  Action = "toggle_archived"
	Machines        Action = "machines"
	Groups          Action = "runs" // config key "runs"; "groups" is a deprecated alias
	Tree            Action = "tree"
	Fold            Action = "fold"
	CollapseAll     Action = "collapse_all"
	ExpandAll       Action = "expand_all"
	Reply           Action = "reply"
	Filter          Action = "filter"
	Search          Action = "search"
	Mark            Action = "mark"
	Visual          Action = "visual"
	Label           Action = "label"
	Rename          Action = "rename"
	Move            Action = "move"
	DetachWin       Action = "detach_window"
	OpenWin         Action = "open_window"
	GroupBy         Action = "group_by"
	Columns         Action = "columns"
	Open            Action = "open"
	OpenArgs        Action = "open_args"
	Kill            Action = "kill"
	Compose         Action = "compose"
	New             Action = "new"
	NewArgs         Action = "new_args"
	Net             Action = "net"
	Quit            Action = "quit"
	Help            Action = "help"
	Bind            Action = "bind"
)

// Def is an action's help metadata and default keys. Group and order drive the
// help layout.
type Def struct {
	Action  Action
	Group   string
	Desc    string
	Default []string
}

// Defs is every configurable action, in help display order.
var Defs = []Def{
	{Down, "move", "move the selection down one line", []string{"j"}},
	{Up, "move", "move the selection up one line", []string{"k"}},
	{Top, "move", "jump to the first row", []string{"g"}},
	{Bottom, "move", "jump to the last row", []string{"G"}},
	{HalfDown, "move", "scroll down half a page", []string{"d"}},
	{HalfUp, "move", "scroll up half a page", []string{"u"}},
	{PreviewDown, "preview", "scroll the preview down one line", []string{"J"}},
	{PreviewUp, "preview", "scroll the preview up one line", []string{"K"}},
	{PreviewHalfDown, "preview", "scroll the preview down half a page", []string{"ctrl-d"}},
	{PreviewHalfUp, "preview", "scroll the preview up half a page", []string{"ctrl-u"}},
	{PreviewTop, "preview", "jump to the oldest line in the preview", []string{"ctrl-g"}},
	{PreviewBottom, "preview", "jump to the newest line in the preview", []string{"ctrl-e"}},
	{NextMatch, "preview", "jump to the next search match", []string{"n"}},
	{PrevMatch, "preview", "jump to the previous search match", []string{"N"}},
	{ColPrev, "sort", "select the column to the left", []string{"H"}},
	{ColNext, "sort", "select the column to the right", []string{"L"}},
	{Sort, "sort", "sort by the selected column, flip if already sorted", []string{"s"}},
	{ColExpand, "sort", "snap the selected column to its full content width, toggle back", []string{"z"}},
	{Scope, "view", "cycle scope: all, live, working, active run", []string{"t"}},
	{Archive, "view", "cycle archive view: unarchived, all, archived", []string{"A"}},
	{Machines, "view", "filter the list by machine", []string{"m"}},
	{Groups, "view", "filter the list to the selected run", []string{"f"}},
	{Tree, "view", "toggle the run tree view", []string{"T"}},
	{GroupBy, "view", "cycle grouping: by directory, run, or tag", []string{"b"}},
	{Columns, "view", "manage columns: show or hide, reorder, resize", []string{"|"}},
	{Fold, "view", "fold or unfold the selected group", []string{"space"}},
	{CollapseAll, "view", "collapse every group", []string{"-"}},
	{ExpandAll, "view", "expand every group", []string{"=", "+"}},
	{Reply, "session", "reply to a session that is waiting for input", []string{"r"}},
	{Filter, "search", "filter the selected column as you type", []string{"i", "a"}},
	{Search, "search", "search the transcript text", []string{"/"}},
	{Mark, "session", "mark or unmark the selected session", []string{"tab"}},
	{Visual, "session", "visual select: j and k extend, v marks the range", []string{"v"}},
	{Label, "session", "tag or untag the selected sessions", []string{"l"}},
	{Rename, "session", "rename the focused session", []string{"R"}},
	{ToggleArchived, "session", "archive or unarchive the selected sessions", []string{"D"}},
	{Move, "session", "move the selection into a tmux session", []string{"M"}},
	{DetachWin, "session", "detach the attached window, keeps the session running", []string{"w"}},
	{OpenWin, "session", "open a window and reattach the selection", []string{"o"}},
	{Open, "session", "resume the session with no extra flags", []string{"e"}},
	{OpenArgs, "session", "resume the session, prompting for flags", []string{"E"}},
	{Kill, "session", "kill the session, stopping it", []string{"x"}},
	{Compose, "session", "compose a launch: harness, mode, then a directory", []string{"c", "C", "ctrl-n"}},
	{New, "session", "start a new session with no extra flags", nil},
	{NewArgs, "session", "start a new session, prompting for flags", nil},
	{Bind, "session", "leader key: run a configured [[bind]] command", []string{"`"}},
	{Net, "session", "network panel: fleet health, sync + rollback profiles", []string{"S"}},
	{Quit, "quit", "quit the picker", []string{"q"}},
	{Help, "quit", "show this help screen", []string{"?"}},
}

// Map resolves a pressed key to an action, and an action back to its keys.
type Map struct {
	toAction map[string]Action
	toKeys   map[Action][]string
}

// legacyActionKeys maps a deprecated [keys] override name to its current
// action, so a config written before the group->run rename ("groups", the old
// name for Groups) still rebinds the right action.
var legacyActionKeys = map[string]Action{
	"groups": Groups,
}

// Build merges user overrides (action name -> keys) onto the defaults. An
// override replaces that action's keys entirely; an unknown or empty action name
// is ignored. On a key bound to two actions, the one later in Defs wins.
func Build(overrides map[string][]string) Map {
	toKeys := make(map[Action][]string, len(Defs))
	for _, d := range Defs {
		ks, ok := overrides[string(d.Action)]
		if !ok {
			for legacy, a := range legacyActionKeys {
				if a == d.Action {
					ks, ok = overrides[legacy]
					break
				}
			}
		}
		if ok && len(ks) > 0 {
			toKeys[d.Action] = ks
		} else {
			toKeys[d.Action] = append([]string(nil), d.Default...)
		}
	}
	toAction := map[string]Action{}
	for _, d := range Defs {
		for _, k := range toKeys[d.Action] {
			toAction[normKey(k)] = d.Action
		}
	}
	return Map{toAction: toAction, toKeys: toKeys}
}

// normKey maps a display notation to the string the picker matches against.
// Only "space" needs translating (to the actual space character); help keeps the
// readable form.
func normKey(k string) string {
	if k == "space" {
		return " "
	}
	return k
}

// NormKey is normKey, exported so a [[bind]] key (config.Bind.Key) is matched
// against a pressed key the same way the picker's own keymap is.
func NormKey(k string) string { return normKey(k) }

// Lookup returns the action a key runs, or "" if the key is unbound.
func (m Map) Lookup(key string) Action { return m.toAction[key] }

// Keys are the keys bound to an action (for help rendering).
func (m Map) Keys(a Action) []string { return m.toKeys[a] }

// Key is an action's primary key, for compact hints ("" if unbound).
func (m Map) Key(a Action) string {
	if ks := m.toKeys[a]; len(ks) > 0 {
		return ks[0]
	}
	return ""
}
