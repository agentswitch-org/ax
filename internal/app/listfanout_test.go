package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/retention"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
	"github.com/agentswitch-org/ax/internal/view"
	"github.com/agentswitch-org/ax/internal/wire"
)

// stubFetchHost swaps the fan-out seam for the duration of a test.
func stubFetchHost(t *testing.T, fn func(config.Host) ([]session.Session, map[string]state.Runtime, string)) {
	t.Helper()
	orig := fetchHostFn
	fetchHostFn = fn
	t.Cleanup(func() { fetchHostFn = orig })
}

// isolatedIndex points HOME/XDG at a temp tree so session.Index scans an empty
// substrate (no real transcripts leak in), then drops one empty claude
// transcript so there is exactly one local session to preserve. It returns that
// session's bare id. It also writes a config with a single [[host]] and points
// AX_CONFIG at it, so the fan-out sees one configured host.
func isolatedIndex(t *testing.T, host string) (localID string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)           // os.UserHomeDir reads USERPROFILE on Windows
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // fresh index cache, no stale real entries
	t.Setenv("XDG_CONFIG_HOME", home)       // isolate: no user notify config fires

	localID = "00000000-0000-0000-0000-0000000000ab"
	proj := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, localID+".jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(home, "config.toml")
	body := "[[host]]\nname = \"" + host + "\"\ntransport = \"ssh " + host + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", cfgPath)
	return localID
}

// The default `ax list` (no --federated) never touches a host, even when one is
// configured: the CLI default stays local-only and fast.
func TestListDefaultDoesNotFanOut(t *testing.T) {
	isolatedIndex(t, "win01")
	calls := 0
	stubFetchHost(t, func(config.Host) ([]session.Session, map[string]state.Runtime, string) {
		calls++
		return nil, nil, view.HostOnline
	})

	captureStdout(t, func() { App{mux: inactiveMux{}}.List(nil) })
	captureStdout(t, func() { App{mux: inactiveMux{}}.List([]string{"--json"}) })

	if calls != 0 {
		t.Fatalf("default list fanned out to a host %d time(s); it must stay local-only", calls)
	}
}

// federatedHosts merges every online host's sessions, host-qualified by
// session.Key, and an offline host degrades to nothing (no hang, no dropped
// rows from the hosts that did answer).
func TestFederatedHostsMergeAndOfflineDegrade(t *testing.T) {
	stubFetchHost(t, func(h config.Host) ([]session.Session, map[string]state.Runtime, string) {
		if h.Name == "win01" {
			return []session.Session{{ID: "abc", Host: "win01"}},
				map[string]state.Runtime{"win01/abc": {State: view.StateLive}}, view.HostOnline
		}
		return nil, nil, view.HostOffline // "dead"
	})

	remote, rt := federatedHostsWithFilter([]config.Host{{Name: "win01"}, {Name: "dead"}}, retention.ActiveOnly)
	if len(remote) != 1 {
		t.Fatalf("only the online host should contribute sessions, got %d", len(remote))
	}
	if got := session.Key(remote[0]); got != "win01/abc" {
		t.Fatalf("remote row not host-qualified, key = %q", got)
	}
	if rt["win01/abc"].State != view.StateLive {
		t.Fatalf("online host runtime state should merge, got %+v", rt)
	}
}

// `ax list --federated --json` emits one wire.Report with the local row keyed
// bare and the remote row host-qualified. The local session is preserved
// alongside the merged remote one.
func TestListFederatedJSONMerges(t *testing.T) {
	localID := isolatedIndex(t, "win01")
	stubFetchHost(t, func(config.Host) ([]session.Session, map[string]state.Runtime, string) {
		return []session.Session{{ID: "abc", Host: "win01", Harness: "claude"}},
			map[string]state.Runtime{"win01/abc": {State: view.StateLive}}, view.HostOnline
	})

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.List([]string{"--federated", "--json"})
	})
	var rep wire.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("federated --json is not a wire.Report: %v\n%s", err, out)
	}
	ids := map[string]bool{}
	for _, s := range rep.Sessions {
		ids[s.ID] = true
	}
	if !ids[localID] {
		t.Fatalf("local session %q dropped from federated report, got ids %v", localID, ids)
	}
	if !ids["win01/abc"] {
		t.Fatalf("remote row not present host-qualified, got ids %v", ids)
	}
	if len(rep.Sessions) != 2 {
		t.Fatalf("federated report should be local + one remote, got %d rows", len(rep.Sessions))
	}
}

// An offline host degrades gracefully: `ax list --federated --json` still emits
// the local report (no hang, local row kept), the remote just contributes
// nothing.
func TestListFederatedJSONOfflineHostKeepsLocal(t *testing.T) {
	localID := isolatedIndex(t, "win01")
	stubFetchHost(t, func(config.Host) ([]session.Session, map[string]state.Runtime, string) {
		return nil, nil, view.HostOffline
	})

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.List([]string{"--federated", "--json"})
	})
	var rep wire.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("federated --json is not a wire.Report: %v\n%s", err, out)
	}
	if len(rep.Sessions) != 1 || rep.Sessions[0].ID != localID {
		t.Fatalf("offline host should leave exactly the local row, got %+v", rep.Sessions)
	}
}

// list --json (the self-report a remote serves) is local-only: it must never reach
// a host, even with one configured, so a federating caller's fetch can never
// trigger a second-level fan-out.
func TestListJSONStaysNonRecursive(t *testing.T) {
	isolatedIndex(t, "win01")
	calls := 0
	stubFetchHost(t, func(config.Host) ([]session.Session, map[string]state.Runtime, string) {
		calls++
		return nil, nil, view.HostOnline
	})

	out := captureStdout(t, func() { App{mux: inactiveMux{}}.List([]string{"--json"}) })
	if calls != 0 {
		t.Fatalf("list --json fanned out %d time(s); the self-report must stay non-recursive", calls)
	}
	if !strings.Contains(out, "\"schema_version\"") {
		t.Fatalf("list --json did not emit a wire.Report:\n%s", out)
	}
}

func TestListJSONCarriesRetentionFields(t *testing.T) {
	localID := isolatedIndex(t, "win01")
	archivedAt := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	if err := meta.Save(localID, meta.Meta{Parent: "root", Archived: true, ArchivedAt: archivedAt}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { App{mux: inactiveMux{}}.List([]string{"--json"}) })
	var hidden wire.Report
	if err := json.Unmarshal([]byte(out), &hidden); err != nil {
		t.Fatalf("list --json is not a wire.Report: %v\n%s", err, out)
	}
	if len(hidden.Sessions) != 0 {
		t.Fatalf("default list --json should hide archived rows, got %+v", hidden.Sessions)
	}

	out = captureStdout(t, func() { App{mux: inactiveMux{}}.List([]string{"--json", "--all"}) })
	var rep wire.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("list --json --all is not a wire.Report: %v\n%s", err, out)
	}
	if rep.SchemaVersion != wire.SchemaVersion || len(rep.Sessions) != 1 {
		t.Fatalf("report shape = v%d/%d sessions", rep.SchemaVersion, len(rep.Sessions))
	}
	got := rep.Sessions[0]
	if got.Lifecycle != state.LifecycleDormant || !got.Archived || !got.Ephemeral || !got.ArchivedAt.Equal(archivedAt) {
		t.Fatalf("retention fields not carried: %+v", got)
	}
}

func TestFederatedHostsHideArchivedByDefault(t *testing.T) {
	stubFetchHost(t, func(h config.Host) ([]session.Session, map[string]state.Runtime, string) {
		ss := []session.Session{
			{ID: "active", Host: h.Name},
			{ID: "archived", Host: h.Name, Archived: true},
		}
		rt := map[string]state.Runtime{
			h.Name + "/active":   {},
			h.Name + "/archived": {},
		}
		return ss, rt, view.HostOnline
	})

	ss, _ := federatedHostsWithFilter([]config.Host{{Name: "win01"}}, retention.ActiveOnly)
	if len(ss) != 1 || ss[0].ID != "active" {
		t.Fatalf("default federatedHosts should hide archived rows, got %+v", ss)
	}
	ss, _ = federatedHostsWithFilter([]config.Host{{Name: "win01"}}, retention.All)
	if len(ss) != 2 {
		t.Fatalf("--all federated host filter should include archived rows, got %+v", ss)
	}
}
