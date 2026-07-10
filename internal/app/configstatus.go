package app

// configstatus.go implements `ax config status`: a read-only fleet view that
// fans out to the configured hosts (reusing the federation transport path),
// reads each node's Capability self-report out of its `ax list --json`, and
// renders a per-host row: reachability + latency, ax/wire version with a compat
// marker, OS/shell, harness set, and profile-in-sync vs drift. It is fail-OPEN:
// an offline / no-ax / version-mismatched host is shown in that state, never
// hangs, and never aborts the other hosts. The self-report it reads is
// local-only (localReport never fans out), so status does not recurse.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/agentswitch-org/ax/internal/build"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/shell"
	"github.com/agentswitch-org/ax/internal/view"
	"github.com/agentswitch-org/ax/internal/wire"
)

// capabilityReport builds the node's own Capability block from its loaded config:
// ax version (build.Version, suffixed with a short commit when the build injected
// one), the wire version it speaks, its harness names, OS class, effective shell,
// effective mux backend, headless-only flag, and its profile hash. It is attached
// to every `ax list --json` self-report (schema v5+).
func capabilityReport(cfg config.Config) *wire.Capability {
	names := make([]string, 0, len(cfg.Harnesses))
	for _, h := range cfg.Harnesses {
		names = append(names, h.Name)
	}
	shellCmd := cfg.Shell
	if shellCmd == "" {
		shellCmd = strings.Join(shell.Prefix(), " ")
	}
	muxBackend := mux.EffectiveName(cfg.Mux)
	ver := build.Version
	if c := build.Commit; c != "" {
		if len(c) > 7 {
			c = c[:7]
		}
		ver = build.Version + "+" + c
	}
	return &wire.Capability{
		AxVersion:   ver,
		WireVersion: wire.SchemaVersion,
		Harnesses:   names,
		OS:          osClass(runtime.GOOS),
		Shell:       shellCmd,
		Mux:         muxBackend,
		Headless:    cfg.Mux == "none", // no multiplexer to hold an interactive session
		ProfileHash: config.ProfileHash(cfg.Profile()),
	}
}

// osClass maps runtime.GOOS to the coarse OS class the capability block reports.
// darwin/linux/windows already ARE their class; any other GOOS passes through
// verbatim rather than being lost.
func osClass(goos string) string {
	switch goos {
	case "darwin", "linux", "windows":
		return goos
	default:
		return goos
	}
}

// fetchHostReportFn is the seam `ax config status` fans out through, so a test
// can inject a canned report/state/latency without an ssh round-trip (like
// fetchHostFn for the picker). Production points it at fetchHostReport.
var fetchHostReportFn = fetchHostReport

// fetchHostReport runs `<transport> <ax> list --json` on h and returns its parsed
// wire.Report, the host's reachability state, and the round-trip latency. Unlike
// fetchHost it does NOT reject an off-version report: the capability block is
// additive/omitempty, so a newer or older but parseable report is "online" and
// the caller marks compatibility from its schema version. Only an unreachable
// transport, a missing ax, or unparseable output is a failure state.
func fetchHostReport(h config.Host) (wire.Report, string, time.Duration) {
	if strings.TrimSpace(h.Transport) == "" {
		return wire.Report{}, view.HostOffline, 0
	}
	prog, argv := transportArgv(h, "list", "--json")
	ctx, cancel := context.WithTimeout(context.Background(), fanoutTimeout)
	defer cancel()
	start := time.Now()
	out, err := exec.CommandContext(ctx, prog, argv...).Output()
	latency := time.Since(start)
	if err != nil {
		var ee *exec.ExitError
		if ctx.Err() != context.DeadlineExceeded && errors.As(err, &ee) && ee.ExitCode() == 127 {
			return wire.Report{}, view.HostNoAx, latency // ssh works, "ax: command not found"
		}
		return wire.Report{}, view.HostOffline, latency
	}
	var rep wire.Report
	if err := json.Unmarshal(out, &rep); err != nil {
		return wire.Report{}, view.HostOffline, latency
	}
	return rep, view.HostOnline, latency
}

// hostStatus is one host's row in the status view (and its --json shape). It is
// view.NetHostStatus, the shape shared with the picker's network panel, so the
// CLI and the panel fold a fetched report the same way (computeHostStatus) and
// never drift apart.
type hostStatus = view.NetHostStatus

// statusReport is the machine-readable envelope `ax config status --json` emits.
type statusReport struct {
	LocalWireVersion int          `json:"local_wire_version"`
	LocalProfileHash string       `json:"local_profile_hash"`
	Hosts            []hostStatus `json:"hosts"`
}

// configStatus fans out to the configured hosts (or one, with --host) and prints
// each host's capability + drift state. Default (no flag) is every configured
// [[host]]; --all is accepted as an explicit synonym. --json emits statusReport.
func (a App) configStatus(args []string) {
	var hostName string
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host":
			if i+1 < len(args) {
				hostName = args[i+1]
				i++
			}
		case "--all":
			// default already targets all configured hosts; accepted for symmetry
		case "--json":
			jsonOut = true
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax config status: %v\n", err)
		os.Exit(1)
	}
	localHash := config.ProfileHash(cfg.Profile())

	var targets []config.Host
	if hostName != "" {
		targets = []config.Host{lookupHost(hostName)}
	} else {
		targets = append(targets, cfg.Hosts...)
		if len(targets) == 0 {
			fmt.Fprintln(os.Stderr, "ax config status: no [[host]] configured (use --host NAME to target one)")
			os.Exit(1)
		}
	}

	rows := gatherHostStatus(targets, localHash)

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(statusReport{
			LocalWireVersion: wire.SchemaVersion,
			LocalProfileHash: localHash,
			Hosts:            rows,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "ax:", err)
			os.Exit(1)
		}
		return
	}
	printStatusTable(rows, localHash)
}

// gatherHostStatus fans out to every target in parallel, fail-open, and returns
// their status rows in target order. Each fetch is bounded by fanoutTimeout, so a
// dead host degrades to an offline row and never blocks the others. This is the
// one fan-out shared by `ax config status` and the picker's network panel (which
// calls computeHostStatus per host off its own UI goroutine); both fold a report
// the same way, so the CLI and the panel can never disagree.
func gatherHostStatus(targets []config.Host, localHash string) []hostStatus {
	rows := make([]hostStatus, len(targets))
	var wg sync.WaitGroup
	for i, h := range targets {
		wg.Add(1)
		go func(i int, h config.Host) {
			defer wg.Done()
			rows[i] = computeHostStatus(h, localHash)
		}(i, h)
	}
	wg.Wait()
	return rows
}

// computeHostStatus fetches h's report and folds it into a status row: an
// unreachable host is marked unreachable/unknown, a reachable one carries its
// version + compat marker, capability fields, and in-sync/drift verdict.
func computeHostStatus(h config.Host, localHash string) hostStatus {
	rep, st, lat := fetchHostReportFn(h)
	row := hostStatus{Host: h.Name, State: st}
	if lat > 0 {
		row.LatencyMS = lat.Milliseconds()
	}
	if st != view.HostOnline {
		row.Compat = "unknown"
		row.Sync = "unreachable"
		return row
	}
	row.Reachable = true
	row.WireVersion = rep.SchemaVersion
	row.Compat = compatMarker(rep.SchemaVersion)
	capb := rep.Capability
	if capb == nil {
		// Reachable and speaks wire, but a pre-v5 ax with no capability block: we
		// cannot read its version or profile hash, so those stay unknown.
		row.Sync = "unknown"
		return row
	}
	row.AxVersion = capb.AxVersion
	row.OS = capb.OS
	row.Shell = capb.Shell
	row.Harnesses = capb.Harnesses
	// A hash match is a STRICT, symmetric in-sync test (the profiles are
	// byte-identical). It deliberately differs from `config sync`'s notion: sync
	// asks "would pushing my profile change the target?", a non-destructive merge,
	// so a target carrying an EXTRA harness/field the sender lacks is "in sync"
	// there but "drift" here. Both are correct; they answer different questions.
	switch {
	case capb.ProfileHash == "":
		row.Sync = "unknown"
	case capb.ProfileHash == localHash:
		row.Sync = "in-sync"
	default:
		row.Sync = "drift"
	}
	return row
}

// compatMarker compares a remote's wire version to the local one.
func compatMarker(remoteWire int) string {
	switch {
	case remoteWire == wire.SchemaVersion:
		return "ok"
	case remoteWire > wire.SchemaVersion:
		return "newer" // remote speaks a newer wire than we understand fully
	default:
		return "older"
	}
}

// printStatusTable renders the human-readable status table.
func printStatusTable(rows []hostStatus, localHash string) {
	fmt.Printf("local: wire v%d  profile %s\n", wire.SchemaVersion, shortHash(localHash))
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tSTATE\tLATENCY\tVERSION\tOS\tSHELL\tHARNESSES\tSYNC")
	for _, r := range rows {
		lat := "-"
		if r.LatencyMS > 0 {
			lat = fmt.Sprintf("%dms", r.LatencyMS)
		}
		ver := "-"
		if r.Reachable {
			v := r.AxVersion
			if v == "" {
				v = "unknown"
			}
			ver = fmt.Sprintf("%s (wire v%d %s)", v, r.WireVersion, r.Compat)
		}
		harnesses := "-"
		if len(r.Harnesses) > 0 {
			harnesses = strings.Join(r.Harnesses, ",")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Host, r.State, lat, ver, dash(r.OS), dash(r.Shell), harnesses, r.Sync)
	}
	w.Flush()
}

// dash renders an empty field as "-".
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortHash truncates a hex hash for display; "none" when empty.
func shortHash(h string) string {
	if h == "" {
		return "none"
	}
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
