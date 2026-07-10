package app

import (
	"fmt"
	"os"
	"strings"

	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/hold"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/runs"
	"github.com/agentswitch-org/ax/internal/session"
)

// Restart tears an existing session down and relaunches it from its persisted
// launch spec (`ax restart <id> [--fresh]`): same behavior/model/task, same mode
// and fence flags, same env and auth policy, pinned back into the same run. A
// fresh session id is minted (a spec relaunch, not a resume), so the new session
// starts from a clean transcript.
//
// --fresh does a clean teardown before reconstructing: it removes the holder socket
// and any process-backend FIFO/pid, so no stale holder or input pipe survives into
// the new session. The dying root's run wrapper would otherwise conclude the run as
// gave_up; restart suppresses that one conclusion so the relaunched session
// continues the run instead of it reading as given up.
//
// Single session only for now: a root or a lone worker, not a whole group.
func (a App) Restart(args []string) {
	fresh := false
	id := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--fresh":
			fresh = true
		default:
			if !strings.HasPrefix(args[i], "-") && id == "" {
				id = args[i]
			}
		}
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: ax restart <id> [--fresh]")
		os.Exit(2)
	}
	id = resolveID(id) // the launch id keeps working for a mint-its-own-id harness

	// Resolve the id against the local index; a remote session cannot be restarted
	// from here (its spec lives on its owner).
	cfg, _ := config.Load()
	var found bool
	for _, s := range session.Index(cfg) {
		if s.ID == id && s.Host == "" {
			found = true
			break
		}
	}
	m := meta.Load(id)
	if m.Spec == nil {
		if !found {
			fmt.Fprintf(os.Stderr, "ax: no local session %q\n", id)
		} else {
			fmt.Fprintf(os.Stderr, "ax: no launch spec for %q; only sessions launched by this ax can be restarted\n", id)
		}
		os.Exit(1)
	}
	sp := m.Spec

	// Suppress the dying wrapper's run conclusion and reopen the run, so the
	// relaunched session continues the same group instead of the restart reading as
	// the run giving up. Set BEFORE the kill so the wrapper sees it as it exits.
	if sp.Group != "" {
		runs.Suppress(sp.Group)
		runs.Remove(sp.Group)
	}

	// Tear the old session down. --fresh also removes the holder socket and any
	// process-backend FIFO/pid, so no held holder or stale input pipe lingers.
	if err := live.Kill(id); err != nil {
		axlog.Printf("restart %s: kill: %v", id, err)
	}
	killCleanup(id)
	if fresh {
		hold.Cleanup(id)
		mux.ProcClear(id)
	}
	meta.Remove(id) // the reconstructed session writes a fresh sidecar under a new id

	o := optsFromSpec(sp)
	a.runLaunch(sp.Harness, o, launchCtx{
		fromRestart: true,
		group:       sp.Group,
		parent:      sp.Parent,
		origin:      sp.Origin,
	})
}
