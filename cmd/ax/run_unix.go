//go:build unix

package main

import (
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/agentswitch-org/ax/internal/app"
	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/hold"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/session"
)

// runHeartbeat wraps a launched harness, invoked as "ax run <id> <command>" by
// the window the picker opens. It runs the harness under a pty proxy so it can
// (a) heartbeat liveness on a timer, (b) mark the session "working" whenever the
// harness writes terminal output (idle when quiet), and (c) clear the record on
// a clean exit. A crash (SIGKILL / power loss) leaves a stale heartbeat, which is
// exactly the crash-recovery signal. If a pty can't be set up it falls back to a
// plain exec (liveness only, no working/idle detection).
func runHeartbeat(args []string) int {
	// Flags precede the id/command. "--adopt <harness>" marks a harness that
	// mints its own session id: the id that follows is a placeholder (the dtach
	// socket key) and the real id is discovered from the index after launch.
	// "--hold" (set on window/attach launches, not --wait/detached ones) keeps
	// the terminal open showing the failure when the harness dies right after
	// launch, so a bad resume reads as an error instead of a flash-closed window.
	adopt, holdFail := "", false
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		switch args[0] {
		case "--hold":
			holdFail, args = true, args[1:]
		case "--adopt":
			if len(args) < 2 {
				return 2
			}
			adopt, args = args[1], args[2:]
		default:
			return 2
		}
	}
	if len(args) < 2 {
		return 2
	}
	id, command := args[0], args[1]
	axlog.Printf("run %s: %s", id, command)
	start := time.Now()

	c := harnessCmd(command)
	logFile := recipeLogFile(id)
	if logFile != nil {
		defer logFile.Close()
	}

	// For an --adopt run, snapshot the harness's existing sessions BEFORE it starts,
	// so discovery reliably sees the one it is about to create as new (a harness
	// that writes its transcript very fast would otherwise already be in the set).
	var before map[string]bool
	if adopt != "" {
		cfg, _ := config.Load()
		before = harnessIDs(session.Index(cfg), adopt)
	}

	ptmx, err := pty.Start(c)
	if err != nil {
		axlog.Printf("run %s: no pty (%v), falling back to plain run", id, err)
		code, tail, stopped := plainRun(id, command)
		app.ConcludeExit(id, code, ansiRe.ReplaceAllString(string(tail), ""), stopped)
		if run := app.RunEnv(); run != "" && os.Getenv("AX_PARENT") == "" {
			app.New(app.Deps{Mux: mux.New()}).ConcludeRun(run)
		}
		return code
	}
	defer ptmx.Close()
	procDone, procErr := waitHarness(c)

	// Native holder: listen on the per-session socket from birth, so a viewer
	// can attach, detach, and reattach this run at any time (an `ax attach`
	// client speaks the framed protocol; closing its window only detaches).
	// The server tees pty output below (ring + attached clients) and feeds
	// client INPUT into the same pty; every other duty of this wrapper
	// (heartbeat, working/idle marks, adopt, fences, conclude) is unchanged
	// whether or not anyone is watching. A failure to take the socket (another
	// holder answering, an unsupported platform) degrades to holding nothing.
	var srv *hold.Server
	if hold.Backend() == hold.BackendNative {
		s, serr := hold.Serve(id, hold.ServeOpts{
			Input:  ptmx,
			Resize: func(rows, cols uint16) { pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols}) },
			Nudge:  func() { winchGroup(c.Process.Pid) },
			Rows:   40,
			Cols:   120,
		})
		if serr != nil {
			axlog.Printf("run %s: hold server: %v", id, serr)
		} else {
			srv = s
			defer srv.Close()
		}
	}

	// Process backend: harness input arrives on a per-session FIFO instead of a
	// terminal (there is none). Read from it in place of os.Stdin, so `ax send`
	// writing to the FIFO reaches the harness pty. Opened O_RDWR so the open never
	// blocks waiting for a writer and reads never see EOF as senders come and go.
	stdin := io.Reader(os.Stdin)
	procMode := false
	if pf := procInput(); pf != nil {
		stdin, procMode = pf, true
		defer pf.Close()
	}

	// track holds the id liveness/output are recorded under. A normal run knows it
	// up front; an --adopt run starts blank and fills it once discoverID finds the
	// session. Until then the PLACEHOLDER id carries the liveness record (started
	// below, handed off at adoption): the placeholder is the only handle the
	// launch printed, and with nothing recorded under it the session was
	// invisible and unkillable for its first seconds, and permanently if
	// discovery never succeeded. beat is the effective id: the adopted real id
	// once known, the placeholder until then.
	track := &tracker{}
	beat := func() string {
		if rid := track.get(); rid != "" {
			return rid
		}
		return id
	}
	live.Start(id, command)
	if adopt == "" {
		track.set(id)
		if procMode {
			if err := mux.ProcTrackPID(id, c.Process.Pid); err != nil {
				axlog.Printf("run %s: track pid: %v", id, err)
			}
		}
	} else {
		// The adopt alias: once discovery finds the harness-minted real id, the
		// native holder simply also listens on the real id's socket (portable to
		// named pipes); the dtach backend keeps the old socket symlink.
		alias := func(realid string) {
			if srv != nil {
				if err := srv.ListenAlso(realid); err != nil {
					axlog.Printf("run %s: alias listener for %s: %v", id, realid, err)
				}
				return
			}
			aliasSock(id, realid)
		}
		go discoverID(adopt, id, command, before, track, c.Process.Pid, procMode, alias)
	}

	done := make(chan struct{})
	defer close(done)
	go heartbeatLoop(beat, done)
	// Conclude a task-carrying interactive worker on a harness with no lifecycle
	// hook (pi, codex) when its own transcript shows the turn ended, the same
	// done-gate claude's Stop hook provides. No-op for hooked or taskless runs.
	// The pty writer is the self-propel pump's injection seam (a --self-propel
	// keep-live coordinator re-invokes itself through it); nil-safe otherwise.
	go watchTurnEnd(beat, func(b []byte) { ptmx.Write(b) }, done)

	// If this session is a run's lifecycle owner (a root with fences), poll the
	// run's cost/token/time totals and cascade-kill on a trip. One poller per
	// run, still no daemon.
	if group, f, ok := app.RunOwner(); ok {
		go app.New(app.Deps{Mux: mux.New()}).WatchFences(group, f, start)
	}

	// Give the harness a usable terminal size immediately. A detached run (a
	// background worker with no client attached) has nothing to inherit a size
	// from, and a 0x0 pty makes Claude Code sit waiting instead of running its
	// task. A real size still arrives via SIGWINCH once someone attaches.
	pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120})
	pty.InheritSize(os.Stdin, ptmx)

	// keep the harness pty sized to our terminal
	watchWinch(func() { pty.InheritSize(os.Stdin, ptmx) })

	if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer term.Restore(int(os.Stdin.Fd()), old)
	}
	// A harness killed mid-run (Ctrl+C) exits without disabling the terminal
	// reporting modes it turned on (mouse tracking, focus, bracketed paste);
	// term.Restore only undoes raw mode, not these DEC private modes, so
	// without this the shell prompt is left printing mouse-motion garbage.
	// Registered after the raw-mode restore defer so it fires first (LIFO),
	// same ordering the hold client uses for the same reset.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		defer os.Stdout.WriteString(hold.ReportingModeReset)
	}
	// Forward stdin to the harness pty. Once the harness has exited and the
	// wrapper is holding a failure on screen, keystrokes become the "dismiss"
	// signal instead (a single reader, so the dismiss keypress can't be lost to
	// a competing read).
	holding := new(atomic.Bool)
	dismiss := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if holding.Load() {
					select {
					case dismiss <- struct{}{}:
					default:
					}
				} else {
					ptmx.Write(buf[:n])
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// A SIGHUP/SIGTERM (from `ax kill`, or a no-dtach window close) means stop:
	// forward it to the harness so it exits and we Remove the record. Then escalate
	// to SIGKILL after a grace period, because a harness that traps SIGTERM (Claude
	// Code does, to guard its session) would otherwise never exit, leaving the tmux
	// window and heartbeat stuck. SIGKILL can't be trapped, so teardown always
	// completes and the window closes when this wrapper returns. ctrl-c is not
	// caught here (raw mode passes it to the harness), so only an uncatchable kill
	// leaves a stale heartbeat (a real crash).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM)
	stopped := new(atomic.Bool) // ax (kill, restart, a fence trip) ended this run, not the harness itself
	go func(pid int) {
		select {
		case <-sig:
			stopped.Store(true)
			if c.Process == nil {
				return
			}
			// Signal the whole process group, not just the direct child: with a
			// configured shell like "zsh -lic" the child is an interactive shell
			// that ignores SIGTERM, and signalling it alone would orphan the
			// harness it spawned. pty.Start setsid's the child, so -pid is its group.
			signalGroup(pid, syscall.SIGTERM)
			t := time.NewTimer(termGrace)
			defer t.Stop()
			select {
			case <-t.C:
				signalGroup(pid, syscall.SIGKILL)
			case <-procDone:
			}
		case <-procDone:
		}
	}(c.Process.Pid)

	// copy harness output to the terminal, marking activity (debounced) as it
	// streams. The read ends when the harness exits or the window closes.
	buf := make([]byte, 32*1024)
	tail := make([]byte, 0, 640) // last chunk of output, logged on a bad exit
	// head is a headless run's early output, scanned for a known-fatal error
	// pattern (unsupported model, invalid request, no credit, missing login) so
	// a doomed run is marked failed the moment it says why, instead of only once
	// its process eventually exits. Bounded: these signatures show up early, so
	// there is no value (and real cost) in scanning a run's entire output.
	const headCap = 4096
	head := make([]byte, 0, headCap)
	failMarked := false
	var lastMark time.Time
	scrubber := &hold.TerminalModeScrubber{}
	for {
		n, rerr := ptmx.Read(buf)
		if n > 0 {
			if out := scrubber.Scrub(buf[:n]); len(out) > 0 {
				os.Stdout.Write(out)
			}
			writeRecipeLog(logFile, buf[:n])
			if srv != nil {
				srv.Output(buf[:n]) // ring (reattach repaint) + attached clients
			}
			tail = keepTail(tail, buf[:n])
			if !failMarked && len(head) < headCap {
				head = append(head, buf[:n]...)
				if len(head) > headCap {
					head = head[:headCap]
				}
				if reason, ok := app.FatalReason(ansiRe.ReplaceAllString(string(head), "")); ok {
					app.MarkFailed(beat(), reason)
					failMarked = true
				}
			}
			if time.Since(lastMark) > time.Second {
				live.Output(beat(), command)
				lastMark = time.Now()
			}
		}
		if rerr != nil {
			break
		}
	}
	<-procDone
	signal.Stop(sig)
	<-procErr
	logExit(id, c, start, tail)
	rid := beat()
	live.Remove(rid)
	if rid != id {
		live.Remove(id)           // the placeholder record, if the adoption handoff raced this exit
		os.Remove(hold.Sock(rid)) // the reattach alias; dtach owns the placeholder socket
	}
	// The wrapper's exit is the universal completion choke point: whatever
	// richer signal fired earlier (a Stop hook, the transcript watcher, a clean
	// headless exit) already left a terminal marker and this is a no-op;
	// otherwise the session died before concluding (a crash, an ax-initiated
	// stop, a harness that quit early) and this records the truthful failure,
	// so a waiter on this id can never hang on a process that no longer exists.
	app.ConcludeExit(rid, exitCode(c), ansiRe.ReplaceAllString(string(tail), ""), stopped.Load())
	if procMode { // remove the input FIFO and pid file (both the launch id and, for an adopt run, the discovered real id)
		mux.ProcClear(id)
		if rid != id {
			mux.ProcClear(rid)
		}
	}
	// A run's root exiting concludes the run: write its record (unless a fence
	// already did). Workers (with a parent) are not run owners.
	if run := app.RunEnv(); run != "" && os.Getenv("AX_PARENT") == "" {
		app.New(app.Deps{Mux: mux.New()}).ConcludeRun(run)
	}
	// Native holder: report the exit to attached clients, who render it (the
	// client-side successor of holdFailure below) and exit. A fresh non-zero
	// exit lingers briefly for the launch window's client racing to attach, so
	// a failed launch is read instead of the socket just vanishing.
	if srv != nil {
		code := exitCode(c)
		srv.Exit(code, time.Since(start), tail, code != 0 && time.Since(start) < holdGrace)
		srv.Close()
	}
	// A non-zero exit within seconds of launch is a failed launch (bad flag,
	// missing binary, unresumable session), and returning would close the window
	// before anyone reads why. Hold it open until a keypress, an ax kill, or the
	// safety timeout. The liveness record is already removed, so the session
	// reads as dead while held.
	if code := exitCode(c); holdFail && code != 0 && time.Since(start) < holdGrace &&
		term.IsTerminal(int(os.Stdin.Fd())) { // no terminal = nobody to dismiss it
		holding.Store(true)
		holdFailure(code, time.Since(start), tail, dismiss)
	}
	return exitCode(c)
}
