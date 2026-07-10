// Package hosts stores dynamic, self-registered host records at
// $XDG_STATE_HOME/ax/hosts/<name>.json, so an ephemeral container can join the
// picker with zero TOML editing: the box drops a record (mounting the dir needs
// no network), and the picker merges it with the static [[host]] config. Each
// record is a heartbeat; a stale one (the box is gone) drops from the list, so
// throwaway boxes clean themselves up.
package hosts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/config"
)

// Fresh is how recently a record must have been written to count as a live host;
// the box re-registers within this, and a gone box ages out.
const Fresh = 5 * time.Minute

// Record is a self-registered host: the same fields as a static [[host]], plus a
// heartbeat timestamp.
type Record struct {
	Name      string    `json:"name"`
	Transport string    `json:"transport"`
	Ax        string    `json:"ax,omitempty"`
	Updated   time.Time `json:"updated"`
}

func dir() string { return axdir.State("hosts") }

func path(name string) string { return filepath.Join(dir(), name+".json") }

// Register writes (or refreshes) a host record, stamping the heartbeat. A box
// calls this on start and on a timer.
func Register(r Record) error {
	r.Updated = time.Now()
	return axdir.WriteJSON(path(r.Name), r)
}

// Deregister removes a host record (clean container shutdown).
func Deregister(name string) { os.Remove(path(name)) }

// List returns every fresh dynamic host as a config.Host, dropping (and deleting)
// stale records so a gone box leaves the picker on its own.
func List() []config.Host {
	es, err := os.ReadDir(dir())
	if err != nil {
		return nil
	}
	var out []config.Host
	for _, e := range es {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir(), e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(data, &r) != nil || r.Name == "" {
			continue
		}
		if time.Since(r.Updated) > Fresh {
			os.Remove(p) // stale: the box is gone
			continue
		}
		out = append(out, config.Host{Name: r.Name, Transport: r.Transport, Ax: r.Ax})
	}
	return out
}

// Merge unions the static hosts with the dynamic ones, static winning on a name
// clash (a durable host is authoritative over a self-registration).
func Merge(static []config.Host) []config.Host {
	seen := map[string]bool{}
	out := append([]config.Host{}, static...)
	for _, h := range static {
		seen[h.Name] = true
	}
	for _, h := range List() {
		if !seen[h.Name] {
			out = append(out, h)
		}
	}
	return out
}
