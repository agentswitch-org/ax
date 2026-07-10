package app

// configsync.go implements the `ax config` verb group: export-profile,
// apply-profile, sync, status, and rollback. sync runs on the key-holder and
// PUSHES the local profile over each host's existing ssh transport into
// `<remote-ax> config apply-profile`, overwriting the target's profile while
// preserving its local settings. status is the read-only fleet view (see
// configstatus.go) and rollback restores a timestamped backup (see
// configrollback.go). See internal/config/profile.go for the profile/local split.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/view"
	"github.com/agentswitch-org/ax/internal/wire"
)

// Config dispatches the `ax config <sub>` verb group.
func (a App) Config(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "ax config: need a subcommand (export-profile | apply-profile | sync | status | rollback)")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "export-profile":
		a.configExportProfile(rest)
	case "apply-profile":
		a.configApplyProfile(rest)
	case "sync":
		a.configSync(rest)
	case "status":
		a.configStatus(rest)
	case "rollback":
		a.configRollback(rest)
	default:
		fmt.Fprintf(os.Stderr, "ax config: unknown subcommand %q (want export-profile | apply-profile | sync | status | rollback)\n", sub)
		os.Exit(2)
	}
}

// configExportProfile prints the local config's portable profile as TOML, after
// linting it for secrets / home paths so a leak never leaves the box.
func (a App) configExportProfile(_ []string) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax config export-profile: %v\n", err)
		os.Exit(1)
	}
	data, err := encodedProfile(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax config export-profile: %v\n", err)
		os.Exit(1)
	}
	os.Stdout.Write(data)
}

// configApplyProfile reads a profile TOML from stdin, diffs it against the local
// config's current effective profile, and (unless --dry-run) merges it into the
// local config: an atomic write with a timestamped backup, preserving every
// local field. It is idempotent: a profile that changes nothing prints the
// in-sync marker and touches no file. This is what runs ON each remote host.
func (a App) configApplyProfile(args []string) {
	dryRun := slices.Contains(args, "--dry-run")
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax config apply-profile: read stdin: %v\n", err)
		os.Exit(1)
	}
	inc, err := config.DecodeProfile(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax config apply-profile: parse profile: %v\n", err)
		os.Exit(1)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax config apply-profile: %v\n", err)
		os.Exit(1)
	}
	diff := config.DiffProfile(cfg.Profile(), inc)
	if len(diff) == 0 {
		fmt.Println(config.InSyncMarker)
		return
	}
	for _, line := range diff {
		fmt.Println(line)
	}
	if dryRun {
		return
	}
	backup, err := config.ApplyProfileToFile(inc, time.Now().UnixNano())
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax config apply-profile: %v\n", err)
		os.Exit(1)
	}
	if backup != "" {
		fmt.Printf("applied (backup %s)\n", backup)
	} else {
		fmt.Println("applied (new config written)")
	}
}

// syncOutcome is one host's result in the final sync summary.
type syncOutcome struct {
	host   string
	status string // "applied" | "in-sync" | "would-change" | "failed"
	detail string
}

// runRemoteConfigFn is the seam `ax config sync` pipes a profile through into a
// host's `config apply-profile`. Production points it at runRemoteConfig; a test
// injects a stub (like listfanout_test does for fetchHostFn) to simulate an
// online/offline host without an ssh round-trip.
var runRemoteConfigFn = runRemoteConfig

// runRemoteConfig pipes payload into `<remote-ax> config apply-profile [--dry-run]`
// on h over its transport (pty-stripped, fail-fast), non-streaming, and returns
// the remote's combined output. An error means the transport or the remote verb
// failed (offline, no ax, bad profile).
func runRemoteConfig(h config.Host, payload []byte, dryRun bool) (string, error) {
	verbArgs := []string{"config", "apply-profile"}
	if dryRun {
		verbArgs = append(verbArgs, "--dry-run")
	}
	prog, argv := transportArgv(remoteTransport(h, false), verbArgs...)
	if prog == "" {
		return "", fmt.Errorf("host %s has no transport", h.Name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteVerbTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, prog, argv...)
	c.Stdin = bytes.NewReader(payload)
	out, err := c.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("transport to %s timed out", h.Name)
	}
	return string(out), err
}

// configCompat is the fail-closed version gate every config mutation runs BEFORE
// pushing a profile into a host's `ax config apply-profile`. It probes h's wire
// self-report (the same fetchHostReportFn path `ax config status` uses) and, if the
// host is reachable but speaks a wire older than wire.ConfigSchemaVersion or answers
// with no capability block, refuses it: that ax predates the `config` verb group and
// a push would only surface a raw "unknown command config". ok is false with an
// actionable reason (no host prefix; the caller adds it) in that case. A host that
// is offline / has no ax is NOT gated here (st != online): it isn't a too-old case,
// so the existing apply path reports its transport error as it always has.
func configCompat(h config.Host) (ok bool, reason string) {
	rep, st, _ := fetchHostReportFn(h)
	if st != view.HostOnline {
		return true, ""
	}
	if rep.SchemaVersion < wire.ConfigSchemaVersion {
		return false, fmt.Sprintf("ax is too old for config sync (reports wire v%d). Update ax on the host to the current build first.", rep.SchemaVersion)
	}
	if rep.Capability == nil {
		return false, "ax is too old for config sync (its wire report carries no capability block). Update ax on the host to the current build first."
	}
	return true, ""
}

// configSync pushes the local profile to one host (--host NAME) or every
// configured host (--all). For each target it first runs apply-profile --dry-run
// to fetch the diff, prints it, and (unless --yes or --dry-run) asks to confirm,
// then applies. Best-effort per host: an offline/unreachable/no-ax host is
// reported and does not abort the others. --dry-run changes nothing anywhere.
func (a App) configSync(args []string) {
	var hostName string
	all, dryRun, yes := false, false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host":
			if i+1 < len(args) {
				hostName = args[i+1]
				i++
			}
		case "--all":
			all = true
		case "--dry-run":
			dryRun = true
		case "--yes", "-y":
			yes = true
		}
	}
	if hostName == "" && !all {
		fmt.Fprintln(os.Stderr, "ax config sync: need --host NAME or --all")
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax config sync: %v\n", err)
		os.Exit(1)
	}
	payload, err := encodedProfile(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax config sync: %v\n", err)
		os.Exit(1)
	}

	var targets []config.Host
	if all {
		// Every explicitly configured [[host]] (not ephemeral self-registered ones).
		targets = append(targets, cfg.Hosts...)
		if len(targets) == 0 {
			fmt.Fprintln(os.Stderr, "ax config sync --all: no [[host]] configured")
			os.Exit(1)
		}
	} else {
		targets = []config.Host{lookupHost(hostName)}
	}

	in := bufio.NewReader(os.Stdin)
	var results []syncOutcome
	for _, h := range targets {
		fmt.Printf("== %s ==\n", h.Name)
		if ok, reason := configCompat(h); !ok {
			msg := h.Name + ": " + reason
			fmt.Fprintln(os.Stderr, msg)
			results = append(results, syncOutcome{h.Name, "skipped", reason})
			continue
		}
		out, err := runRemoteConfigFn(h, payload, true)
		if err != nil {
			trimmed := strings.TrimSpace(out)
			if trimmed != "" {
				fmt.Fprintln(os.Stderr, trimmed)
			}
			fmt.Fprintf(os.Stderr, "ax: host %s: %v\n", h.Name, err)
			results = append(results, syncOutcome{h.Name, "failed", err.Error()})
			continue
		}
		fmt.Print(ensureNewline(out))
		if strings.Contains(out, config.InSyncMarker) {
			results = append(results, syncOutcome{h.Name, "in-sync", ""})
			continue
		}
		if dryRun {
			results = append(results, syncOutcome{h.Name, "would-change", ""})
			continue
		}
		if !yes {
			fmt.Printf("apply to %s? [y/N] ", h.Name)
			line, _ := in.ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				fmt.Printf("skipped %s\n", h.Name)
				results = append(results, syncOutcome{h.Name, "failed", "declined"})
				continue
			}
		}
		aout, aerr := runRemoteConfigFn(h, payload, false)
		fmt.Print(ensureNewline(aout))
		if aerr != nil {
			fmt.Fprintf(os.Stderr, "ax: host %s: %v\n", h.Name, aerr)
			results = append(results, syncOutcome{h.Name, "failed", aerr.Error()})
			continue
		}
		results = append(results, syncOutcome{h.Name, "applied", ""})
	}

	printSyncSummary(results)
}

// syncPayload loads the local config and returns its linted, encoded profile,
// ready to push. Shared by the CLI sync and the picker network panel so both push
// the same lint-gated bytes; a secret / home path is refused before any host is
// touched.
func (a App) syncPayload() ([]byte, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return encodedProfile(cfg)
}

func encodedProfile(cfg config.Config) ([]byte, error) {
	p := cfg.Profile()
	if err := config.LintProfile(p); err != nil {
		return nil, err
	}
	return config.EncodeProfile(p)
}

// pushProfileTo applies payload to h non-interactively via the same
// runRemoteConfigFn seam `ax config sync` uses, and returns a one-line outcome
// ("applied" / "in-sync" / "failed: ..."). Used by the picker's network panel,
// which confirms in-TUI before calling this instead of asking on stdin.
func (a App) pushProfileTo(h config.Host, payload []byte) string {
	out, err := runRemoteConfigFn(h, payload, false)
	if err != nil {
		if t := strings.TrimSpace(out); t != "" {
			return "failed: " + lastLine(t)
		}
		return "failed: " + err.Error()
	}
	if strings.Contains(out, config.InSyncMarker) {
		return "in-sync"
	}
	return "applied"
}

// netSyncHost is the network panel's sync-to-one-host action: build the payload
// (once, lint-gated) and push it to h. Reuses the CLI's sync mechanism verbatim.
func (a App) netSyncHost(h config.Host) string {
	if ok, reason := configCompat(h); !ok {
		return "skipped: " + reason
	}
	payload, err := a.syncPayload()
	if err != nil {
		return "failed: " + err.Error()
	}
	return a.pushProfileTo(h, payload)
}

// netSyncAll is the panel's sync-to-all action: push the one linted payload to
// every configured host in turn (best-effort, an offline host is counted failed
// and does not abort the rest) and return a compact tally.
func (a App) netSyncAll(hosts []config.Host) string {
	payload, err := a.syncPayload()
	if err != nil {
		return "failed: " + err.Error()
	}
	var applied, inSync, failed, skipped int
	for _, h := range hosts {
		if ok, _ := configCompat(h); !ok {
			skipped++
			continue
		}
		switch r := a.pushProfileTo(h, payload); {
		case r == "in-sync":
			inSync++
		case strings.HasPrefix(r, "failed"):
			failed++
		default:
			applied++
		}
	}
	return fmt.Sprintf("applied=%d in-sync=%d failed=%d skipped=%d", applied, inSync, failed, skipped)
}

// lastLine is the last non-empty line of s, trimmed; "" when s is blank. The
// panel shows a remote's most telling line (its final status/error) on one row.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// ensureNewline returns s with a trailing newline, so a remote's un-terminated
// output does not run into the next line.
func ensureNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// printSyncSummary prints the per-host applied / in-sync / would-change / failed
// tally at the end of a sync run.
func printSyncSummary(results []syncOutcome) {
	var applied, inSync, would, failed, skipped int
	fmt.Println("-- summary --")
	for _, r := range results {
		switch r.status {
		case "applied":
			applied++
		case "in-sync":
			inSync++
		case "would-change":
			would++
		case "failed":
			failed++
		case "skipped":
			skipped++
		}
		line := fmt.Sprintf("  %-16s %s", r.host, r.status)
		if r.detail != "" {
			line += ": " + r.detail
		}
		fmt.Println(line)
	}
	fmt.Printf("applied=%d in-sync=%d would-change=%d failed=%d skipped=%d\n", applied, inSync, would, failed, skipped)
}
