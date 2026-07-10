package app

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/view"
	"github.com/agentswitch-org/ax/internal/wire"
)

// stubRunRemoteConfig swaps the sync transport seam for the duration of a test.
func stubRunRemoteConfig(t *testing.T, fn func(config.Host, []byte, bool) (string, error)) {
	t.Helper()
	orig := runRemoteConfigFn
	runRemoteConfigFn = fn
	t.Cleanup(func() { runRemoteConfigFn = orig })
}

// stubAllHostsCurrent stubs the compat-probe seam so every host reports a current,
// config-capable wire report. Sync/rollback now gate on this before pushing, so a
// test that exercises the push path must supply a report the gate accepts.
func stubAllHostsCurrent(t *testing.T) {
	t.Helper()
	stubFetchHostReport(t, func(config.Host) (wire.Report, string, time.Duration) {
		return onlineReport("x"), view.HostOnline, time.Millisecond
	})
}

// isolatedSyncCfg points AX_CONFIG/HOME/XDG at temp dirs with a config that
// declares the given hosts, so config.Load()/lookupHost see exactly them and no
// real user config leaks in.
func isolatedSyncCfg(t *testing.T, hosts ...string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // no self-registered hosts folded in
	var b strings.Builder
	b.WriteString("default_harness = \"claude\"\n")
	for _, h := range hosts {
		b.WriteString("\n[[host]]\nname = \"" + h + "\"\ntransport = \"ssh " + h + "\"\n")
	}
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_CONFIG", path)
}

// TestSyncHostTargetsOne: `--host win01 --yes` contacts only win01 and applies.
func TestSyncHostTargetsOne(t *testing.T) {
	isolatedSyncCfg(t, "win01", "other")
	stubAllHostsCurrent(t)
	var mu sync.Mutex
	seen := map[string]int{}
	stubRunRemoteConfig(t, func(h config.Host, _ []byte, dry bool) (string, error) {
		mu.Lock()
		seen[h.Name]++
		mu.Unlock()
		if dry {
			return "~ default_harness changed\n", nil
		}
		return "applied\n", nil
	})

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"sync", "--host", "win01", "--yes"})
	})
	if seen["other"] != 0 {
		t.Fatalf("--host win01 must not contact other hosts, saw %v", seen)
	}
	if seen["win01"] == 0 {
		t.Fatalf("win01 was never contacted, saw %v", seen)
	}
	if !strings.Contains(out, "applied=1") {
		t.Fatalf("summary should report one applied host:\n%s", out)
	}
}

// TestSyncAllTargetsEveryHost: `--all --yes` contacts every configured host.
func TestSyncAllTargetsEveryHost(t *testing.T) {
	isolatedSyncCfg(t, "a", "b", "c")
	stubAllHostsCurrent(t)
	var mu sync.Mutex
	seen := map[string]bool{}
	stubRunRemoteConfig(t, func(h config.Host, _ []byte, dry bool) (string, error) {
		mu.Lock()
		seen[h.Name] = true
		mu.Unlock()
		if dry {
			return "~ change\n", nil
		}
		return "applied\n", nil
	})

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"sync", "--all", "--yes"})
	})
	for _, h := range []string{"a", "b", "c"} {
		if !seen[h] {
			t.Fatalf("--all missed host %q, saw %v", h, seen)
		}
	}
	if !strings.Contains(out, "applied=3") {
		t.Fatalf("summary should report three applied hosts:\n%s", out)
	}
}

// TestSyncDryRunChangesNothing: `--all --dry-run` fetches every host's diff but
// never runs a real (non-dry) apply.
func TestSyncDryRunChangesNothing(t *testing.T) {
	isolatedSyncCfg(t, "a", "b")
	stubAllHostsCurrent(t)
	var mu sync.Mutex
	applied := 0
	stubRunRemoteConfig(t, func(_ config.Host, _ []byte, dry bool) (string, error) {
		if !dry {
			mu.Lock()
			applied++
			mu.Unlock()
		}
		return "~ change\n", nil
	})

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"sync", "--all", "--dry-run"})
	})
	if applied != 0 {
		t.Fatalf("--dry-run must never apply, ran %d applies", applied)
	}
	if !strings.Contains(out, "would-change=2") {
		t.Fatalf("dry-run summary should count would-change hosts:\n%s", out)
	}
}

// TestSyncOfflineHostDoesNotDropOthers: one host errors (offline); the other is
// still applied and the failure is reported, not fatal.
func TestSyncOfflineHostDoesNotDropOthers(t *testing.T) {
	isolatedSyncCfg(t, "dead", "live")
	// The offline host is offline at the probe too, but an offline host is NOT the
	// too-old case: the gate passes it through so the apply path reports its
	// transport failure exactly as before.
	stubFetchHostReport(t, func(h config.Host) (wire.Report, string, time.Duration) {
		if h.Name == "dead" {
			return wire.Report{}, view.HostOffline, 0
		}
		return onlineReport("x"), view.HostOnline, time.Millisecond
	})
	stubRunRemoteConfig(t, func(h config.Host, _ []byte, dry bool) (string, error) {
		if h.Name == "dead" {
			return "", errOffline
		}
		if dry {
			return "~ change\n", nil
		}
		return "applied\n", nil
	})

	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"sync", "--all", "--yes"})
	})
	if !strings.Contains(out, "applied=1") || !strings.Contains(out, "failed=1") {
		t.Fatalf("offline host should be reported failed while the live one applies:\n%s", out)
	}
}

// TestSyncInSyncHostReported: a host already in sync (remote prints the marker)
// is counted in-sync and never applied.
func TestSyncInSyncHostReported(t *testing.T) {
	isolatedSyncCfg(t, "synced")
	stubAllHostsCurrent(t)
	applied := 0
	stubRunRemoteConfig(t, func(_ config.Host, _ []byte, dry bool) (string, error) {
		if !dry {
			applied++
		}
		return config.InSyncMarker + "\n", nil
	})
	out := captureStdout(t, func() {
		App{mux: inactiveMux{}}.Config([]string{"sync", "--all", "--yes"})
	})
	if applied != 0 {
		t.Fatalf("an in-sync host must not be applied")
	}
	if !strings.Contains(out, "in-sync=1") {
		t.Fatalf("summary should report the in-sync host:\n%s", out)
	}
}

// TestSyncSkipsTooOldHost: a reachable host whose ax predates the config verb
// group (wire v4, no capability block) is FAILED CLOSED. It is skipped with a clear,
// actionable message BEFORE any apply-profile push, while a current host alongside
// it still syncs. This is the whole point of the gate: never draw a raw "unknown
// command config" from an old remote.
func TestSyncSkipsTooOldHost(t *testing.T) {
	isolatedSyncCfg(t, "oldbox", "newbox")
	stubFetchHostReport(t, func(h config.Host) (wire.Report, string, time.Duration) {
		if h.Name == "oldbox" {
			return wire.Report{SchemaVersion: 4}, view.HostOnline, time.Millisecond // pre-config ax
		}
		return onlineReport("x"), view.HostOnline, time.Millisecond
	})
	var pushed map[string]bool
	var mu sync.Mutex
	pushed = map[string]bool{}
	stubRunRemoteConfig(t, func(h config.Host, _ []byte, dry bool) (string, error) {
		mu.Lock()
		pushed[h.Name] = true
		mu.Unlock()
		if dry {
			return "~ change\n", nil
		}
		return "applied\n", nil
	})

	var out string
	stderr := captureStderr(t, func() {
		out = captureStdout(t, func() {
			App{mux: inactiveMux{}}.Config([]string{"sync", "--all", "--yes"})
		})
	})

	if pushed["oldbox"] {
		t.Fatalf("a too-old host must NEVER be pushed to; apply-profile was called on oldbox")
	}
	if !pushed["newbox"] {
		t.Fatalf("the current host should still be synced")
	}
	wantMsg := "oldbox: ax is too old for config sync (reports wire v4). Update ax on the host to the current build first."
	if !strings.Contains(stderr, wantMsg) {
		t.Fatalf("expected the actionable skip message on stderr, got:\n%s", stderr)
	}
	if !strings.Contains(out, "applied=1") || !strings.Contains(out, "skipped=1") {
		t.Fatalf("summary should apply the current host and skip the old one:\n%s", out)
	}
}

// TestNetSyncPanelSkipsTooOldHost: the picker network panel's sync actions run the
// same gate. netSyncHost on a too-old host returns a skipped line and pushes nothing;
// netSyncAll tallies the old host as skipped and the current one as applied.
func TestNetSyncPanelSkipsTooOldHost(t *testing.T) {
	isolatedSyncCfg(t, "oldbox", "newbox")
	stubFetchHostReport(t, func(h config.Host) (wire.Report, string, time.Duration) {
		if h.Name == "oldbox" {
			return wire.Report{SchemaVersion: 4}, view.HostOnline, time.Millisecond
		}
		return onlineReport("x"), view.HostOnline, time.Millisecond
	})
	pushed := map[string]bool{}
	var mu sync.Mutex
	stubRunRemoteConfig(t, func(h config.Host, _ []byte, dry bool) (string, error) {
		mu.Lock()
		pushed[h.Name] = true
		mu.Unlock()
		return "applied\n", nil
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	a := App{mux: inactiveMux{}}

	old := a.netSyncHost(lookupHost("oldbox"))
	if !strings.HasPrefix(old, "skipped:") || !strings.Contains(old, "too old for config sync") {
		t.Fatalf("panel sync of a too-old host should be skipped with a reason, got %q", old)
	}
	if pushed["oldbox"] {
		t.Fatalf("panel must not push to a too-old host")
	}

	tally := a.netSyncAll(cfg.Hosts)
	if !strings.Contains(tally, "applied=1") || !strings.Contains(tally, "skipped=1") {
		t.Fatalf("sync-all tally should apply the current host and skip the old one, got %q", tally)
	}
}

// errOffline is a stand-in transport failure for the offline-host test.
var errOffline = offlineErr{}

type offlineErr struct{}

func (offlineErr) Error() string { return "transport failed" }
