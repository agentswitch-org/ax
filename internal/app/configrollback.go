package app

// configrollback.go implements `ax config rollback`: the recovery counterpart to
// apply-profile. apply-profile snapshots config.toml.bak.<numeric-suffix> before every
// overwrite; rollback restores the MOST RECENT such backup over the current
// config (atomic: temp in the same dir + rename), printing which backup it chose
// and a diff of what changes. It confirms first unless --yes; with no backup it
// says so and changes nothing. `--host NAME` runs the rollback on a remote over
// the transport.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/agentswitch-org/ax/internal/config"
)

// configRollback dispatches rollback to the local config or, with --host NAME, to
// a remote over the transport.
func (a App) configRollback(args []string) {
	var hostName string
	yes := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host":
			if i+1 < len(args) {
				hostName = args[i+1]
				i++
			}
		case "--yes", "-y":
			yes = true
		}
	}
	if hostName != "" {
		a.configRollbackRemote(hostName, yes)
		return
	}
	a.configRollbackLocal(yes)
}

// configRollbackLocal restores the newest config.toml.bak.<numeric-suffix> over the
// local config. It prints the chosen backup and a profile-level diff, confirms
// (unless yes), then restores atomically. No backup, or a backup identical to the
// current config, is a no-op that touches nothing.
func (a App) configRollbackLocal(yes bool) {
	backup, ts, ok := config.LatestBackup()
	if !ok {
		fmt.Println("ax config rollback: no backup found; nothing to roll back")
		return
	}
	current, err := os.ReadFile(config.Path())
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "ax config rollback: read current config: %v\n", err)
		os.Exit(1)
	}
	backupData, err := os.ReadFile(backup)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax config rollback: read backup: %v\n", err)
		os.Exit(1)
	}
	if bytes.Equal(current, backupData) {
		fmt.Printf("current config already matches the latest backup (%s); nothing to roll back\n", filepath.Base(backup))
		return
	}

	fmt.Printf("restoring %s (suffix %d)\n", filepath.Base(backup), ts)
	// The backup is the pre-apply snapshot and apply-profile only ever changes
	// profile fields, so a profile-level diff (current -> backup) is what the
	// rollback will actually revert. Reuses the same DiffProfile the sync path uses.
	curP, _ := config.ProfileFromBytes(current)
	bakP, _ := config.ProfileFromBytes(backupData)
	diff := config.DiffProfile(curP, bakP)
	if len(diff) == 0 {
		fmt.Println("  (no profile-field differences; the change is in local-only fields)")
	} else {
		for _, line := range diff {
			fmt.Println("  " + line)
		}
	}

	if !yes {
		fmt.Print("roll back to this backup? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			fmt.Println("rollback aborted; config unchanged")
			return
		}
	}
	if err := config.RestoreBackup(backup); err != nil {
		fmt.Fprintf(os.Stderr, "ax config rollback: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("rolled back to %s\n", filepath.Base(backup))
}

// configRollbackRemote confirms locally (unless yes), then reruns `ax config
// rollback --yes` on the host over its transport, streaming the remote's output
// and propagating its exit code. The remote does the actual restore
// non-interactively (its own backup is the source of truth on that box).
func (a App) configRollbackRemote(hostName string, yes bool) {
	h := lookupHost(hostName)
	if ok, reason := configCompat(h); !ok {
		fmt.Fprintf(os.Stderr, "%s: %s\n", hostName, reason)
		os.Exit(1)
	}
	if !yes {
		fmt.Printf("roll back config on %s to its latest backup? [y/N] ", hostName)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			fmt.Println("rollback aborted; remote config unchanged")
			return
		}
	}
	a.remoteVerb("config", hostName, []string{"rollback", "--yes"}, false)
}

// netRollbackHost is the network panel's rollback action: rerun `ax config
// rollback --yes` on h over its transport, capturing the output rather than
// streaming it (the panel owns the screen and confirms in-TUI, so it cannot use
// configRollbackRemote's stdin/stdout path). It reuses remoteArgv, the same
// transport-and-quote builder remoteVerb uses, and returns the remote's most
// telling line ("rolled back to ...", or "no backup found ..."). Fail-open: an
// unreachable host returns a "failed: ..." line, never hangs the panel.
func (a App) netRollbackHost(h config.Host) string {
	if ok, reason := configCompat(h); !ok {
		return "skipped: " + reason
	}
	prog, argv := remoteArgv(h, "config", []string{"rollback", "--yes"}, false)
	if prog == "" {
		return "failed: host " + h.Name + " has no transport"
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteVerbTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, prog, argv...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "failed: transport to " + h.Name + " timed out"
	}
	if err != nil {
		if t := lastLine(string(out)); t != "" {
			return "failed: " + t
		}
		return "failed: " + err.Error()
	}
	if t := lastLine(string(out)); t != "" {
		return t
	}
	return "rolled back"
}
