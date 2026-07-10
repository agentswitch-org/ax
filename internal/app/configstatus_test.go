package app

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/shell"
	"github.com/agentswitch-org/ax/internal/state"
	"github.com/agentswitch-org/ax/internal/view"
	"github.com/agentswitch-org/ax/internal/wire"
)

// stubFetchHostReport swaps the status fan-out seam for the duration of a test.
func stubFetchHostReport(t *testing.T, fn func(config.Host) (wire.Report, string, time.Duration)) {
	t.Helper()
	orig := fetchHostReportFn
	fetchHostReportFn = fn
	t.Cleanup(func() { fetchHostReportFn = orig })
}

// localProfileHash loads the isolated test config and returns the hash `ax config
// status` will compute for the local side, so a stub can hand a host a matching
// (in-sync) or differing (drift) hash.
func localProfileHash(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return config.ProfileHash(cfg.Profile())
}

// onlineReport is a minimal online wire.Report carrying a capability block with
// the given profile hash.
func onlineReport(profileHash string) wire.Report {
	return wire.Report{
		SchemaVersion: wire.SchemaVersion,
		Capability: &wire.Capability{
			AxVersion:   "test-1.0",
			WireVersion: wire.SchemaVersion,
			Harnesses:   []string{"claude"},
			OS:          "linux",
			Shell:       "sh -c",
			ProfileHash: profileHash,
		},
	}
}

// TestStatusInSyncVsDrift: a host whose reported profile hash equals the local
// hash is in-sync; a host whose hash differs is drift.
func TestStatusInSyncVsDrift(t *testing.T) {
	isolatedSyncCfg(t, "synced", "drifted")
	local := localProfileHash(t)
	stubFetchHostReport(t, func(h config.Host) (wire.Report, string, time.Duration) {
		if h.Name == "synced" {
			return onlineReport(local), view.HostOnline, 5 * time.Millisecond
		}
		return onlineReport("deadbeef-different"), view.HostOnline, 5 * time.Millisecond
	})

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"status", "--json"})
	})
	var rep statusReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("status --json not parseable: %v\n%s", err, out)
	}
	sync := map[string]string{}
	for _, h := range rep.Hosts {
		sync[h.Host] = h.Sync
	}
	if sync["synced"] != "in-sync" {
		t.Fatalf("matching hash should be in-sync, got %q", sync["synced"])
	}
	if sync["drifted"] != "drift" {
		t.Fatalf("differing hash should be drift, got %q", sync["drifted"])
	}
}

// TestStatusOfflineHostGraceful: an offline host is shown as unreachable and
// never aborts the run or drops the other host's row.
func TestStatusOfflineHostGraceful(t *testing.T) {
	isolatedSyncCfg(t, "dead", "live")
	local := localProfileHash(t)
	stubFetchHostReport(t, func(h config.Host) (wire.Report, string, time.Duration) {
		if h.Name == "dead" {
			return wire.Report{}, view.HostOffline, 0
		}
		return onlineReport(local), view.HostOnline, 3 * time.Millisecond
	})

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"status", "--json"})
	})
	var rep statusReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("status --json not parseable: %v\n%s", err, out)
	}
	byName := map[string]hostStatus{}
	for _, h := range rep.Hosts {
		byName[h.Host] = h
	}
	if d := byName["dead"]; d.Reachable || d.Sync != "unreachable" || d.State != view.HostOffline {
		t.Fatalf("offline host mishandled: %+v", d)
	}
	if l := byName["live"]; !l.Reachable || l.Sync != "in-sync" {
		t.Fatalf("live host should still be reported in-sync alongside a dead one: %+v", l)
	}
}

// TestStatusHostVsAllTargeting: `--host NAME` contacts exactly one host; the
// default (no flag) contacts every configured host.
func TestStatusHostVsAllTargeting(t *testing.T) {
	isolatedSyncCfg(t, "a", "b", "c")
	var mu sync.Mutex
	seen := map[string]int{}
	stubFetchHostReport(t, func(h config.Host) (wire.Report, string, time.Duration) {
		mu.Lock()
		seen[h.Name]++
		mu.Unlock()
		return onlineReport("x"), view.HostOnline, time.Millisecond
	})

	captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"status", "--host", "b", "--json"})
	})
	if seen["a"] != 0 || seen["c"] != 0 || seen["b"] != 1 {
		t.Fatalf("--host b must contact only b, saw %v", seen)
	}

	seen = map[string]int{}
	captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"status", "--json"})
	})
	for _, h := range []string{"a", "b", "c"} {
		if seen[h] != 1 {
			t.Fatalf("default status must contact every host once, saw %v", seen)
		}
	}
}

// TestStatusPreV5HostUnknownSync: a reachable host that answers WITHOUT a
// capability block (a pre-v5 ax) is reachable but its sync state is unknown, not
// a false in-sync or drift.
func TestStatusPreV5HostUnknownSync(t *testing.T) {
	isolatedSyncCfg(t, "oldbox")
	stubFetchHostReport(t, func(config.Host) (wire.Report, string, time.Duration) {
		return wire.Report{SchemaVersion: 4}, view.HostOnline, 2 * time.Millisecond
	})

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"status", "--json"})
	})
	var rep statusReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("status --json not parseable: %v\n%s", err, out)
	}
	if len(rep.Hosts) != 1 {
		t.Fatalf("expected one host row, got %d", len(rep.Hosts))
	}
	h := rep.Hosts[0]
	if !h.Reachable || h.Sync != "unknown" || h.Compat != "older" {
		t.Fatalf("pre-v5 host should be reachable/unknown-sync/older-compat, got %+v", h)
	}
}

// TestGatherHostStatusRows: the shared status fan-out returns one row per host in
// target order, folding an online host into a version + in-sync/drift row and an
// offline host into an unreachable row. This is the one code path the picker's
// network panel and `ax config status` share, so a change to the fold hits both.
func TestGatherHostStatusRows(t *testing.T) {
	isolatedSyncCfg(t, "alpha", "bravo")
	local := localProfileHash(t)
	stubFetchHostReport(t, func(h config.Host) (wire.Report, string, time.Duration) {
		if h.Name == "bravo" {
			return wire.Report{}, view.HostOffline, 0
		}
		return onlineReport(local), view.HostOnline, 9 * time.Millisecond
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	rows := gatherHostStatus(cfg.Hosts, local)
	if len(rows) != 2 {
		t.Fatalf("want a row per host (2), got %d", len(rows))
	}
	if rows[0].Host != "alpha" || rows[1].Host != "bravo" {
		t.Fatalf("rows must keep target order, got %q then %q", rows[0].Host, rows[1].Host)
	}
	if a := rows[0]; !a.Reachable || a.Sync != "in-sync" || a.State != view.HostOnline ||
		a.LatencyMS != 9 || a.AxVersion != "test-1.0" || a.Compat != "ok" {
		t.Fatalf("online host folded wrong: %+v", a)
	}
	if b := rows[1]; b.Reachable || b.Sync != "unreachable" || b.State != view.HostOffline {
		t.Fatalf("offline host must be an unreachable row, not a gap: %+v", b)
	}
}

// TestListJSONIncludesCapability: the local self-report carries a capability
// block (so a federating status caller has data to consume), and localReport is
// still non-recursive (it never reaches a host to build the block).
func TestListJSONIncludesCapability(t *testing.T) {
	isolatedIndex(t, "win01")
	calls := 0
	stubFetchHost(t, func(config.Host) ([]session.Session, map[string]state.Runtime, string) {
		calls++
		return nil, nil, view.HostOnline
	})
	out := captureStdout(t, func() { App{mux: inactiveMux{}}.List([]string{"--json"}) })
	if calls != 0 {
		t.Fatalf("building the capability block must not fan out to a host, saw %d", calls)
	}
	var rep wire.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("list --json not a wire.Report: %v\n%s", err, out)
	}
	if rep.Capability == nil {
		t.Fatalf("local self-report is missing its capability block:\n%s", out)
	}
	if rep.Capability.WireVersion != wire.SchemaVersion || rep.Capability.ProfileHash == "" {
		t.Fatalf("capability block underpopulated: %+v", rep.Capability)
	}
	if rep.Capability.Shell != strings.Join(shell.Prefix(), " ") || rep.Capability.Mux != mux.EffectiveName("") {
		t.Fatalf("capability defaults = shell %q mux %q, want shell %q mux %q",
			rep.Capability.Shell, rep.Capability.Mux, strings.Join(shell.Prefix(), " "), mux.EffectiveName(""))
	}
	if !strings.Contains(out, "\"profile_hash\"") {
		t.Fatalf("capability block should serialize a profile_hash:\n%s", out)
	}
}
