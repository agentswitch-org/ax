//go:build windows

package main

import (
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/agentswitch-org/ax/internal/app"
	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/hold"
	"github.com/agentswitch-org/ax/internal/hold/conpty"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/shell"
)

// runHeartbeat wraps a launched harness, invoked as "ax run <id> <command>":
// the Windows twin of the unix pty wrapper, with the harness under a ConPTY
// (internal/hold/conpty) instead of creack/pty. Same duties: heartbeat
// liveness, working/idle marks on output, the native holder endpoint from
// birth, and the truthful exit record. The lifecycle differs where ConPTY
// does: exit is detected by Wait (not a pty read error; conhost keeps the
// output pipe open), teardown is drain-ordered (Close flushes the console
// while the read loop keeps draining, whose EOF then ends it), and `ax kill`
// terminates this wrapper outright (Windows has no SIGTERM to forward), which
// tears the ConPTY and its process tree down with it. If a ConPTY can't be
// set up it falls back to a plain exec (liveness only).
func runHeartbeat(args []string) int {
	// Flags precede the id/command, exactly as on unix: "--adopt <harness>"
	// marks a mint-its-own-id harness, "--hold" keeps a fast failure on screen.
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

	// Clear any inherited ctrl-c-ignore attribute BEFORE spawning the harness,
	// or a chain launched by a non-pty ssh exec inherits it into the ConPTY and
	// `ax send --interrupt` becomes a silent no-op (see conpty.EnableCtrlC).
	conpty.EnableCtrlC()

	// For an --adopt run, snapshot the harness's existing sessions BEFORE it
	// starts, so discovery reliably sees the one it creates as new.
	var before map[string]bool
	if adopt != "" {
		cfg, _ := config.Load()
		before = harnessIDs(session.Index(cfg), adopt)
	}
	logFile := recipeLogFile(id)
	if logFile != nil {
		defer logFile.Close()
	}

	rows, cols := uint16(40), uint16(120)
	if r, c, ok := hold.TerminalSize(int(os.Stdin.Fd())); ok {
		rows, cols = r, c
	}
	p, err := conpty.Start(harnessArgv(command), conpty.Options{Rows: rows, Cols: cols})
	if err != nil {
		axlog.Printf("run %s: no conpty (%v), falling back to plain run", id, err)
		code, tail, stopped := plainRun(id, command)
		app.ConcludeExit(id, code, ansiRe.ReplaceAllString(string(tail), ""), stopped)
		if run := app.RunEnv(); run != "" && os.Getenv("AX_PARENT") == "" {
			app.New(app.Deps{Mux: mux.New()}).ConcludeRun(run)
		}
		return code
	}
	defer p.Close()

	// Native holder: listen on the per-session named pipe from birth, so a
	// viewer can attach, detach, and reattach this run at any time. Resize goes
	// straight to the pseudoconsole (ConPTY repaints the program itself; no
	// SIGWINCH exists to relay), and the reattach nudge is its resize wiggle.
	var srv *hold.Server
	if hold.Backend() == hold.BackendNative {
		s, serr := hold.Serve(id, hold.ServeOpts{
			Input:  p,
			Resize: func(rows, cols uint16) { p.Resize(rows, cols) },
			Nudge:  p.Nudge,
			Rows:   rows,
			Cols:   cols,
		})
		if serr != nil {
			axlog.Printf("run %s: hold server: %v", id, serr)
		} else {
			srv = s
			defer srv.Close()
		}
	}

	// track/beat: the id liveness and output are recorded under; an --adopt run
	// hands off from the placeholder once discovery finds the real id.
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
	} else {
		// The adopt alias: the holder also listens on the real id's pipe, so an
		// attach by either id lands on this process (no symlink analog exists
		// for named pipes, and none is needed).
		alias := func(realid string) {
			if srv != nil {
				if err := srv.ListenAlso(realid); err != nil {
					axlog.Printf("run %s: alias listener for %s: %v", id, realid, err)
				}
			}
		}
		go discoverID(adopt, id, command, before, track, p.Pid(), false, alias)
	}

	done := make(chan struct{})
	defer close(done)
	go heartbeatLoop(beat, done)
	// The ConPTY writer is the self-propel pump's injection seam (the only
	// platform difference from unix); nil-safe when the pump is not engaged.
	go watchTurnEnd(beat, func(b []byte) { p.Write(b) }, done)

	// If this session is a run's lifecycle owner (a root with fences), poll the
	// run's totals and cascade-kill on a trip.
	if group, f, ok := app.RunOwner(); ok {
		go app.New(app.Deps{Mux: mux.New()}).WatchFences(group, f, start)
	}

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
	// Forward stdin to the pseudoconsole. Once the harness has exited and the
	// wrapper is holding a failure on screen, keystrokes become the "dismiss"
	// signal instead (a single reader, so the dismiss keypress can't be lost).
	holding := new(atomic.Bool)
	dismiss := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if holding.Load() {
					select {
					case dismiss <- struct{}{}:
					default:
					}
				} else {
					p.Write(buf[:n])
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// The only deliverable stop signal on Windows is a console ctrl event
	// (os.Interrupt); an `ax kill` TerminateProcesses this wrapper instead, and
	// the dying wrapper's ConPTY handles tear the harness tree down. Kill the
	// harness on interrupt so teardown still records a truthful, intentional
	// stop when a console event does arrive.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	stopped := new(atomic.Bool) // ax ended this run, not the harness itself
	go func() {
		<-sig
		stopped.Store(true)
		p.Kill()
	}()

	// Exit is detected by Wait, not by the read loop (conhost holds the output
	// pipe open across the program's exit). Close after Wait flushes the last
	// output into the still-draining read loop below and EOFs it: the
	// drain-ordered teardown the ConPTY contract requires.
	exited := make(chan int, 1)
	go func() {
		code, werr := p.Wait()
		if werr != nil {
			axlog.Printf("run %s: wait: %v", id, werr)
		}
		exited <- code
		p.Close()
	}()

	// copy harness output to stdout, marking activity (debounced) as it
	// streams; same tee (ring + attached clients) and fatal-pattern scan as unix.
	buf := make([]byte, 32*1024)
	tail := make([]byte, 0, 640) // last chunk of output, logged on a bad exit
	const headCap = 4096
	head := make([]byte, 0, headCap)
	failMarked := false
	var lastMark time.Time
	scrubber := &hold.TerminalModeScrubber{}
	for {
		n, rerr := p.Read(buf)
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
	code := <-exited
	if stopped.Load() {
		code = 0 // an intentional stop, not a harness failure (unix maps a TERM/KILL death the same way)
	}
	dur := time.Since(start).Round(time.Millisecond)
	if code != 0 {
		axlog.Printf("run %s exited: code=%d after=%s; last output: %q", id, code, dur, strings.TrimSpace(string(tail)))
	} else {
		axlog.Printf("run %s exited: code=%d after=%s", id, code, dur)
	}
	rid := beat()
	live.Remove(rid)
	if rid != id {
		live.Remove(id) // the placeholder record, if the adoption handoff raced this exit
	}
	// The wrapper's exit is the universal completion choke point: record the
	// truthful failure unless a richer signal already concluded the session.
	app.ConcludeExit(rid, code, ansiRe.ReplaceAllString(string(tail), ""), stopped.Load())
	// A run's root exiting concludes the run: write its record (unless a fence
	// already did). Workers (with a parent) are not run owners.
	if run := app.RunEnv(); run != "" && os.Getenv("AX_PARENT") == "" {
		app.New(app.Deps{Mux: mux.New()}).ConcludeRun(run)
	}
	// Native holder: report the exit to attached clients; a fresh non-zero exit
	// lingers briefly for the launch window's client racing to attach.
	if srv != nil {
		srv.Exit(code, time.Since(start), tail, code != 0 && time.Since(start) < holdGrace)
		srv.Close()
	}
	// A non-zero exit within seconds of launch is a failed launch; hold it on
	// screen until a keypress or the safety timeout, as on unix.
	if holdFail && code != 0 && time.Since(start) < holdGrace &&
		term.IsTerminal(int(os.Stdin.Fd())) { // no terminal = nobody to dismiss it
		holding.Store(true)
		holdFailure(code, time.Since(start), tail, dismiss)
	}
	return code
}

// harnessArgv builds the argv that runs a harness under the ConPTY, through
// the shell configured in `shell` (default pwsh/powershell -NoProfile
// -Command): the Windows analog of harnessCmd, as ConPTY spawns by
// CreateProcess with an explicit argv rather than an exec.Cmd.
func harnessArgv(command string) []string {
	cfg, _ := config.Load()
	fields := strings.Fields(cfg.Shell)
	if len(fields) == 0 {
		return append(shell.Prefix(), command)
	}
	return append(append([]string{}, fields...), command)
}
