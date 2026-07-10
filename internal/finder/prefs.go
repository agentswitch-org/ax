package finder

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/retention"
)

// uiPrefs is picker UI state that should survive between popup invocations,
// stored as JSON under ~/.local/state/ax/ui.json.
type uiPrefs struct {
	Scope   int    `json:"scope"`             // scopeAll/scopeLive/scopeWorking
	GroupBy string `json:"groupBy,omitempty"` // "" | "dir" | "run" | "host" | "tag:<key>"
	Archive int    `json:"archive,omitempty"` // retention.ActiveOnly/All/ArchivedOnly

	// Collapsed holds the collapsed group keys for the pivot named by GroupBy.
	// Collapse keys are only meaningful under the pivot they were collapsed
	// in, so the two fields are always saved and loaded together.
	Collapsed []string `json:"collapsed,omitempty"`

	// ActiveOnly is the legacy pre-tri-state field: a bare active-only toggle.
	// Still read so an old ui.json keeps its intent (true -> scopeLive) on the
	// first launch after upgrade; never written anymore.
	ActiveOnly bool `json:"activeOnly,omitempty"`

	// Columns is the saved column-management layout: every column in horizontal
	// order with its visibility and width, written on the modal's OK. Empty means
	// no saved layout, so the config (or built-in) defaults apply.
	Columns []colPref `json:"columns,omitempty"`
}

// colPref is one column's persisted layout entry (see uiPrefs.Columns): its
// stable key, whether it is shown, and its display width.
type colPref struct {
	Key     string `json:"key"`
	Visible bool   `json:"visible"`
	Width   int    `json:"width"`
}

// collapsedFor returns the persisted collapsed set if it was saved under the
// given pivot, and an empty set otherwise. This is what stops a GroupBy
// switch from applying one pivot's fold state to another.
func (p uiPrefs) collapsedFor(groupBy string) map[string]bool {
	set := map[string]bool{}
	if p.GroupBy != groupBy {
		return set
	}
	for _, k := range p.Collapsed {
		set[k] = true
	}
	return set
}

// scope resolves the persisted preference into a scopeMode, migrating the legacy
// activeOnly flag when the new scope field is absent.
func (p uiPrefs) scope() scopeMode {
	if p.Scope == 0 && p.ActiveOnly {
		return scopeLive
	}
	return scopeMode(p.Scope)
}

func (p uiPrefs) archive() retention.ArchiveFilter {
	switch retention.ArchiveFilter(p.Archive) {
	case retention.All, retention.ArchivedOnly:
		return retention.ArchiveFilter(p.Archive)
	default:
		return retention.ActiveOnly
	}
}

func (p uiPrefs) groupBy() string {
	if p.GroupBy == "" {
		return "run"
	}
	return p.GroupBy
}

func prefsPath() string {
	return axdir.StatePath("ui.json")
}

func loadPrefs() uiPrefs {
	var p uiPrefs
	if data, err := os.ReadFile(prefsPath()); err == nil {
		json.Unmarshal(data, &p)
		return p
	}
	return uiPrefs{}
}

func savePrefs(p uiPrefs) {
	fp := prefsPath()
	if os.MkdirAll(filepath.Dir(fp), 0o700) != nil {
		return
	}
	if data, err := json.Marshal(p); err == nil {
		os.WriteFile(fp, data, 0o600)
	}
}
