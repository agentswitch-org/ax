// Command ax (agentswitch) is a session switcher: a popup over every past
// session across all configured LLM CLIs (claude, pi, ...), with resume and
// launch. main wires the tool implementations into the app and dispatches.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agentswitch-org/ax/internal/app"
	"github.com/agentswitch-org/ax/internal/axlog"
	"github.com/agentswitch-org/ax/internal/build"
	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/dirs"
	"github.com/agentswitch-org/ax/internal/finder"
	"github.com/agentswitch-org/ax/internal/hold"
	"github.com/agentswitch-org/ax/internal/live"
	"github.com/agentswitch-org/ax/internal/meta"
	"github.com/agentswitch-org/ax/internal/mux"
	"github.com/agentswitch-org/ax/internal/notify"
	"github.com/agentswitch-org/ax/internal/propel"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/shell"
	"github.com/agentswitch-org/ax/internal/state"
)

func main() {
	rawArgs := os.Args[1:]

	// "run" is the heartbeat/pty wrapper a launched window runs around the
	// harness; it is not a user command. Its exit code is the harness's, so a
	// --wait caller (and CI) sees a failed run as a failure.
	if len(rawArgs) > 0 && rawArgs[0] == "run" {
		os.Exit(runHeartbeat(rawArgs[1:]))
	}

	m := mux.New()
	a := app.New(app.Deps{
		Find: finder.New(m),
		Mux:  m,
		Dirs: dirs.New(),
	})

	cfg, _ := config.Load()
	action, name, args := classifyCmd(rawArgs, cfg)

	switch action {
	case actionPick:
		a.Pick()
	case actionHarness:
		a.Launch(name, args)
	case actionExtension:
		if tryExtension(name, args) {
			return // unreachable on success; the exec replaces this process
		}
		// tryExtension only returns false when LookPath misses, but classifyCmd
		// already confirmed the binary exists; if exec itself fails it prints and
		// exits inside tryExtension, so we only reach here on a race.
		fmt.Fprintf(os.Stderr, "ax: exec ax-%s failed\n", name)
		os.Exit(1)
	case actionDefault:
		// default_harness is set: treat the first positional as a prompt.
		// `ax "some task"` becomes `ax <default_harness> "some task"`.
		a.Launch(name, args)
	case actionUnknown:
		if v := nearVerb(name); v != "" {
			fmt.Fprintf(os.Stderr, "ax: unknown command %q; did you mean %q?\n", name, v)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "ax: unknown command %q; set default_harness in config to run a prompt directly, or use ax <harness> \"<prompt>\"\n", name)
		usage()
		os.Exit(2)
	case actionVerb:
		switch name {
		case "pick":
			a.Pick()
		case "new":
			a.New(newArgs(args))
		case "list":
			a.List(args)
		case "attach":
			a.Attach(args)
		case "preview":
			a.Preview(args)
		case "search":
			a.Search(args)
		case "kill":
			// --run cascade-kills a whole run (workers before the root); any ids
			// alongside it are killed too rather than silently skipping the cascade.
			g, rest := app.GroupArg(args)
			if g != "" {
				a.KillGroup(g)
			}
			if len(rest) > 0 {
				a.Kill(rest)
			}
		case "archive":
			a.Archive(args)
		case "unarchive":
			a.Unarchive(args)
		case "prune":
			a.Prune(args)
		case "tag":
			a.Tag(args)
		case "read":
			a.Read(args)
		case "result":
			a.Result(args)
		case "wait":
			a.Wait(args)
		case "send":
			a.Send(args)
		case "ask":
			a.Ask(args)
		case "reply":
			a.Reply(args)
		case "move":
			a.Move(args)
		case "restart":
			a.Restart(args)
		case "continue":
			a.Continue(args)
		case "runs":
			a.Runs(args)
		case "metrics":
			a.Metrics(args)
		case "host":
			a.Host(args)
		case "config":
			a.Config(args)
		case "hook":
			a.Hook(args)
		case "hookstate":
			a.HookState(args)
		case "await-close":
			// Internal: the detached --close-on-done closer closeSession spawns from
			// inside a harness's Stop hook. Not a user command, so not in usage.
			a.AwaitClose(args)
		case "reap-worker":
			// Internal: delayed process reap for concluded workers. Not a user command.
			a.ReapWorker(args)
		case "fence-check":
			a.FenceCheck(args)
		case "check":
			a.Check(args)
		case "log":
			os.Stdout.Write(axlog.Dump())
		case "models":
			app.UpdateModels(cfg, args)
		case "version", "--version":
			v := "ax " + build.Display()
			if build.Commit != "" || build.Date != "" {
				detail := build.Commit
				if build.Date != "" {
					if detail != "" {
						detail += ", built " + build.Date
					} else {
						detail = "built " + build.Date
					}
				}
				v += " (" + detail + ")"
			}
			fmt.Println(v)
		case "help", "-h", "--help":
			usage()
		}
	}
}

// tryExtension looks for an `ax-<cmd>` executable on PATH and, when it finds
// one, replaces this process with it (execvp, like git-foo/kubectl-foo),
// forwarding the remaining args. It reports false only when no such executable
// exists, so the caller can print the unknown-command error; on success it does
// not return. Built-in verbs are matched before this in the switch, so an
// extension can add verbs but never shadow a built-in. The extension inherits
// ax's environment (including AX_SESSION_ID/AX_RUN when ax runs inside a
// session) plus AX, the absolute path to this binary, so it can call back in
// (`"$AX" list --json`) without assuming ax is on PATH under that name.
func tryExtension(cmd string, args []string) bool {
	path, err := exec.LookPath("ax-" + cmd)
	if err != nil {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	argv := append([]string{path}, args...)
	if err := execReplace(path, argv, extensionEnv(os.Environ(), self)); err != nil {
		fmt.Fprintf(os.Stderr, "ax: exec %s: %v\n", path, err)
		os.Exit(1)
	}
	return true
}

// extensionEnv returns env with AX set to self, replacing any inherited AX (a
// nested `ax foo` whose extension itself runs `ax bar`) rather than appending a
// duplicate, since execve leaves duplicate-key resolution unspecified.
func extensionEnv(env []string, self string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "AX=") {
			out = append(out, kv)
		}
	}
	return append(out, "AX="+self)
}

// dispatchAction is the classification result from classifyCmd.
type dispatchAction int

const (
	actionPick      dispatchAction = iota // no args or first arg starts with -
	actionVerb                            // known built-in verb
	actionHarness                         // known configured harness
	actionExtension                       // ax-<cmd> executable on PATH
	actionDefault                         // unknown arg but default_harness is set
	actionUnknown                         // unknown arg and no default_harness
)

// classifyCmd classifies the dispatch for the given args and config.
// It returns the action, the effective command or harness name, and the
// final args slice to pass through to the handler. For actionDefault,
// name is the default harness and finalArgs prepends the original cmd
// so the call is equivalent to `ax <default_harness> <original-cmd> <rest...>`.
func classifyCmd(args []string, cfg config.Config) (action dispatchAction, name string, finalArgs []string) {
	cmd := "pick"
	rest := args
	if len(args) > 0 && (!strings.HasPrefix(args[0], "-") || isKnownVerb(args[0])) {
		cmd, rest = args[0], args[1:]
	}
	if cmd == "pick" {
		return actionPick, "", rest
	}
	if isKnownVerb(cmd) {
		return actionVerb, cmd, rest
	}
	for _, h := range cfg.Harnesses {
		if h.Name == cmd {
			return actionHarness, cmd, rest
		}
	}
	if _, err := exec.LookPath("ax-" + cmd); err == nil {
		return actionExtension, cmd, rest
	}
	if cfg.DefaultHarness != "" {
		// A lone bare word one edit away from a real verb is far more likely a
		// typo ("ax lst", "ax kil abc") than a one-word prompt; launching a
		// session on it would silently burn a run on garbage. Refuse with the
		// suggestion; a quoted or multi-word prompt never trips this.
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			if v := nearVerb(cmd); v != "" {
				return actionUnknown, cmd, rest
			}
		}
		return actionDefault, cfg.DefaultHarness, append([]string{cmd}, rest...)
	}
	return actionUnknown, cmd, rest
}

// nearVerb returns the built-in verb within edit distance 1 of cmd, or "".
func nearVerb(cmd string) string {
	if len(cmd) < 3 {
		return "" // too short to judge; "ax go" stays a prompt
	}
	for _, v := range []string{
		"pick", "new", "list", "attach", "preview", "search", "kill",
		"archive", "unarchive", "prune", "tag", "read", "result", "wait", "send", "ask", "reply", "move",
		"restart", "continue", "runs", "metrics", "host", "config", "hook",
		"check", "log", "models", "version", "help",
	} {
		if editDistanceAtMost1(cmd, v) {
			return v
		}
	}
	return ""
}

// editDistanceAtMost1 reports whether a and b are within one insertion,
// deletion, or substitution of each other (and not equal; equal never reaches
// here because known verbs match first).
func editDistanceAtMost1(a, b string) bool {
	la, lb := len(a), len(b)
	if la > lb {
		a, b, la, lb = b, a, lb, la
	}
	if lb-la > 1 {
		return false
	}
	i, j, diffs := 0, 0, 0
	for i < la && j < lb {
		if a[i] == b[j] {
			i++
			j++
			continue
		}
		diffs++
		if diffs > 1 {
			return false
		}
		if la == lb {
			i++ // substitution
		}
		j++ // insertion into the shorter string
	}
	return diffs+(lb-j)-(la-i) <= 1
}

// isKnownVerb reports whether cmd is a built-in ax verb (not a harness name).
func isKnownVerb(cmd string) bool {
	switch cmd {
	case "run", "pick", "new", "list", "attach", "preview", "search", "kill",
		"archive", "unarchive", "prune", "tag", "read", "result", "wait", "send", "ask", "reply", "move",
		"restart", "continue", "runs", "metrics", "host", "config", "hook", "hookstate",
		"await-close", "reap-worker", "fence-check", "check", "log", "models",
		"version", "--version", "help", "-h", "--help":
		return true
	}
	return false
}

// termGrace is how long the run wrapper waits after forwarding SIGTERM before it
// SIGKILLs a harness that refuses to exit, so `ax kill` always tears the session
// (and its window) down.
const termGrace = 3 * time.Second

// plainOutputDrainGrace bounds how long the no-pty fallback waits for its
// stdout/stderr copy loops after the direct harness has exited and cleanup has
// run. A descendant can keep inherited fds open forever, but teardown cannot
// wait behind that.
const plainOutputDrainGrace = time.Second

// holdGrace bounds what counts as a launch failure: a harness that ran longer
// than this and then exited non-zero is a session ending, not a launch that
// never got off the ground, and its window should close as usual.
const holdGrace = 15 * time.Second

// holdTimeout auto-dismisses a held failure, so a held wrapper whose window was
// already closed (nobody can ever press a key) does not linger forever.
const holdTimeout = 15 * time.Minute

// ansiRe strips terminal escape sequences from captured output, so replaying
// the failure tail prints as text instead of re-driving the terminal.
var ansiRe = regexp.MustCompile(`\x1b(\][^\a\x1b]*(\a|\x1b\\)|\[[0-9;?<>=$ ]*[a-zA-Z@]|[()][A-Z0-9]|.)`)

// holdFailure prints why the harness died and blocks until dismissed. The
// harness's last words are replayed (stripped of escape codes) because a
// full-screen harness often clears them on teardown; the same tail is already
// in the ax log.
func holdFailure(code int, dur time.Duration, tail []byte, dismiss <-chan struct{}) {
	last := strings.TrimSpace(ansiRe.ReplaceAllString(string(tail), ""))
	fmt.Printf("\r\n\x1b[31max: harness exited with code %d after %s\x1b[0m\r\n", code, dur.Round(time.Millisecond))
	if last != "" {
		for _, line := range strings.Split(last, "\n") {
			fmt.Printf("  %s\r\n", strings.TrimRight(line, "\r"))
		}
	}
	fmt.Printf("\x1b[2msee `ax log`; press any key to close\x1b[0m\r\n")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
	select {
	case <-dismiss:
	case <-sig:
	case <-time.After(holdTimeout):
	}
}

// exitCode is the harness's exit status, so the wrapper (and any --wait caller)
// reports a failed run as a failure instead of always exiting 0. A harness ax
// itself stopped (TERM/HUP/INT, or ax's own SIGKILL escalation) reads as 0; any
// other signal (a SIGSEGV, an OOM kill) is a crash and reads as 128+sig.
func exitCode(c *exec.Cmd) int {
	if c.ProcessState == nil {
		return 0
	}
	if code := c.ProcessState.ExitCode(); code >= 0 {
		return code
	}
	if sig, ok := waitSignal(c); ok {
		switch sig {
		case syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGKILL:
			return 0 // an intentional stop, not a harness failure
		default:
			return 128 + int(sig)
		}
	}
	return 0
}

// harnessCmd builds the command that runs a harness, through the shell configured
// in `shell` (default "sh -c"). A login+interactive shell like "zsh -lic" makes
// tool managers (mise, nvm, asdf) resolve when ax's own environment does not
// already have them, e.g. when ax is invoked over ssh.
func harnessCmd(command string) *exec.Cmd {
	cfg, _ := config.Load()
	fields := strings.Fields(cfg.Shell)
	if len(fields) == 0 {
		return shell.Command(command)
	}
	args := append(append([]string{}, fields[1:]...), command)
	return exec.Command(fields[0], args...)
}

// procInput opens this run's process-backend input FIFO, or returns nil when the
// run is not under the process backend (AX_PROC_FIFO unset). The path is set by
// mux.process.Open; this creates the FIFO if it does not exist yet and opens it
// O_RDWR so the open never blocks on a missing writer and reads never hit EOF
// between senders. `ax send` writes text into the same FIFO.
func procInput() *os.File {
	path := os.Getenv("AX_PROC_FIFO")
	if path == "" {
		return nil
	}
	if err := makeFIFO(path); err != nil && !os.IsExist(err) {
		axlog.Printf("run: mkfifo %s: %v", path, err)
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		axlog.Printf("run: open fifo %s: %v", path, err)
		return nil
	}
	return f
}

// keepTail appends p to tail, keeping only the last 512 bytes.
func keepTail(tail, p []byte) []byte {
	tail = append(tail, p...)
	if len(tail) > 512 {
		tail = tail[len(tail)-512:]
	}
	return tail
}

// logExit records how a session ended: the mapped exit code (an intentional
// SIGTERM/SIGKILL stop reads as 0, not the raw -1 of a signaled death, which
// used to make every clean close-on-done teardown look like a crash in this
// log), plus the signal when there was one. A non-zero, fast exit is the
// signal that the harness failed to launch (e.g. not found), and the tail
// carries the reason.
func logExit(id string, c *exec.Cmd, start time.Time, tail []byte) {
	code := exitCode(c)
	sig := ""
	if s, ok := waitSignal(c); ok {
		sig = " signal=" + s.String()
	}
	dur := time.Since(start).Round(time.Millisecond)
	if code != 0 {
		axlog.Printf("run %s exited: code=%d%s after=%s; last output: %q", id, code, sig, dur, strings.TrimSpace(string(tail)))
	} else {
		axlog.Printf("run %s exited: code=%d%s after=%s", id, code, sig, dur)
	}
}

// plainRun is the no-pty fallback: liveness heartbeat only.
func plainRun(id, command string) (int, []byte, bool) {
	live.Start(id, command)
	done := make(chan struct{})
	defer close(done)
	go heartbeatLoop(func() string { return id }, done)

	logFile := recipeLogFile(id)
	if logFile != nil {
		defer logFile.Close()
	}
	tail := &tailCapture{}
	c := harnessCmd(command)
	c.Stdin = os.Stdin
	stdout, stderr := []io.Writer{os.Stdout, tail}, []io.Writer{os.Stderr, tail}
	if logFile != nil {
		stdout = append(stdout, logFile)
		stderr = append(stderr, logFile)
	}
	stdoutPipe, err := newPlainOutputPipe(io.MultiWriter(stdout...))
	if err != nil {
		axlog.Printf("run %s: stdout pipe: %v", id, err)
		live.Remove(id)
		return 1, nil, false
	}
	stderrPipe, err := newPlainOutputPipe(io.MultiWriter(stderr...))
	if err != nil {
		stdoutPipe.closeWriter()
		stdoutPipe.closeReader()
		axlog.Printf("run %s: stderr pipe: %v", id, err)
		live.Remove(id)
		return 1, nil, false
	}
	c.Stdout = stdoutPipe.write
	c.Stderr = stderrPipe.write
	// Process backend: take input from the session FIFO instead of os.Stdin (which
	// is /dev/null here), so `ax send` still reaches the harness without a pty.
	if pf := procInput(); pf != nil {
		c.Stdin = pf
		defer pf.Close()
	}
	preparePlainRun(c)
	if err := c.Start(); err != nil {
		stdoutPipe.closeWriter()
		stdoutPipe.closeReader()
		stderrPipe.closeWriter()
		stderrPipe.closeReader()
		axlog.Printf("run %s: start failed: %v", id, err)
		live.Remove(id)
		return 1, nil, false
	}
	stdoutPipe.closeWriter()
	stderrPipe.closeWriter()
	if os.Getenv("AX_PROC_FIFO") != "" {
		mux.ProcTrackPID(id, c.Process.Pid)
		defer mux.ProcClear(id)
	}
	procDone, procErr := waitHarness(c)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	stopped := new(atomic.Bool)
	go func(pid int) {
		select {
		case <-sig:
			stopped.Store(true)
			stopPlainRun(c, syscall.SIGTERM)
			t := time.NewTimer(termGrace)
			defer t.Stop()
			select {
			case <-t.C:
				stopPlainRun(c, syscall.SIGKILL)
			case <-procDone:
			}
		case <-procDone:
		}
	}(c.Process.Pid)
	<-procDone
	signal.Stop(sig)
	<-procErr
	waitPlainOutputDrains(plainOutputDrainGrace, stdoutPipe, stderrPipe)
	live.Remove(id)
	return exitCode(c), tail.Bytes(), stopped.Load()
}

type plainOutputPipe struct {
	read  *os.File
	write *os.File
	done  chan struct{}
}

func newPlainOutputPipe(dst io.Writer) (*plainOutputPipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p := &plainOutputPipe{read: r, write: w, done: make(chan struct{})}
	go func() {
		_, _ = io.Copy(dst, r)
		_ = r.Close()
		close(p.done)
	}()
	return p, nil
}

func (p *plainOutputPipe) closeWriter() {
	if p != nil && p.write != nil {
		_ = p.write.Close()
	}
}

func (p *plainOutputPipe) closeReader() {
	if p != nil && p.read != nil {
		_ = p.read.Close()
	}
}

func waitPlainOutputDrains(timeout time.Duration, pipes ...*plainOutputPipe) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for _, p := range pipes {
		if p == nil {
			continue
		}
		select {
		case <-p.done:
		case <-t.C:
			for _, stuck := range pipes {
				stuck.closeReader()
			}
			return
		}
	}
}

func waitHarness(c *exec.Cmd) (<-chan struct{}, <-chan error) {
	done := make(chan struct{})
	errc := make(chan error, 1)
	pid := 0
	if c.Process != nil {
		pid = c.Process.Pid
	}
	go func() {
		err := c.Wait()
		cleanupProcessGroup(pid)
		errc <- err
		close(done)
	}()
	return done, errc
}

type tailCapture struct {
	mu   sync.Mutex
	tail []byte
}

func (t *tailCapture) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.tail = keepTail(t.tail, p)
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailCapture) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.tail...)
}

func recipeLogFile(id string) *os.File {
	m := meta.Load(id)
	if m.LogPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.LogPath), 0o700); err != nil {
		axlog.Printf("run %s: recipe log mkdir: %v", id, err)
		return nil
	}
	f, err := os.OpenFile(m.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		axlog.Printf("run %s: recipe log open: %v", id, err)
		return nil
	}
	return f
}

func writeRecipeLog(f *os.File, p []byte) {
	if f != nil && len(p) > 0 {
		_, _ = f.Write(p)
	}
}

// turnEndPoll is how often the transcript watcher re-checks a hookless
// harness's transcript for a turn end. The check is mtime-gated, so an idle
// transcript costs one stat per tick.
const turnEndPoll = 2 * time.Second

// watchTurnEnd concludes a task-carrying interactive worker on a harness with
// no lifecycle hook (pi, codex) when its transcript shows the agent's turn
// ended, mirroring claude's Stop -> `ax hookstate stop` done-gate. Without
// this, an interactive pi or codex worker NEVER reaches the done state: nothing
// observes its turn end, so `ax wait` blocks forever and the launch is
// fire-and-forget by construction. The transcript is the harness's own record
// of its turn lifecycle (pi stamps stopReason on each message, codex logs
// task_started/task_complete events), so this is authoritative, not a
// pane-scrape. get returns the effective session id (the adopted real id once
// known); the watcher retires itself for hooked harnesses (claude reports via
// its own hook), headless runs with no stale terminal marker (their exit
// concludes them), and taskless sessions (a human at the wheel).
// watchTurnEnd's write is the seam into the wrapper's own pty (ptmx.Write on
// unix, ConPTY p.Write on Windows): the self-propel pump injects a
// continue-prompt through it to re-invoke an idle inline coordinator. nil on a
// path with no pty (the plain-run fallback never calls this), which disables the
// pump; the conclude path is unchanged there.
func watchTurnEnd(get func() string, write func([]byte), done <-chan struct{}) {
	t := time.NewTicker(turnEndPoll)
	defer t.Stop()
	var format, file string
	var lastMod time.Time
	var prop *propel.Propeller // non-nil only for a --self-propel keep-live pi/codex session
	reopenOnly := false
	first := true
	for {
		if first {
			first = false
		} else {
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
		id := get()
		if id == "" {
			continue
		}
		if format == "" {
			m := meta.Load(id)
			if m.Mode == "" {
				continue // sidecar not written or not yet migrated by adoption
			}
			if strings.TrimSpace(m.Task) == "" {
				return // a human-driven session
			}
			switch m.Mode {
			case "interactive":
			case "headless":
				if !state.Terminal(id) {
					return // ordinary headless run: exit concludes it
				}
				reopenOnly = true
			default:
				return
			}
			cfg, _ := config.Load()
			for _, h := range cfg.Harnesses {
				if h.Name == m.Harness {
					format = h.Format
				}
			}
			if format != "pi" && format != "codex" {
				return // hooked (claude) or unwatchable: the hook or exit is authoritative
			}
			// Build the self-propel pump only for an opted-in keep-live session with
			// a pty to inject into. Everything else (no flag, no pty, non-keep-live)
			// keeps the unchanged conclude-on-turn-end behavior below.
			if !reopenOnly && write != nil && m.KeepLive && m.Spec != nil && m.Spec.SelfPropel {
				prop = newPropeller(get, write, m.Dir, m.Group)
			}
		}
		if file == "" {
			cfg, _ := config.Load()
			for _, s := range session.Index(cfg) {
				if s.ID == id && s.Host == "" {
					file = s.File
				}
			}
			if file == "" {
				continue // transcript not written yet
			}
		}
		if prop != nil {
			// The pump's timer hook: wakes a session parked on live workers once
			// they finish, and runs the submit watchdog (an inject with no
			// transcript activity counts as an idle turn so the cap still advances).
			prop.Tick()
		}
		info, err := os.Stat(file)
		if err != nil || !info.ModTime().After(lastMod) {
			continue
		}
		lastMod = info.ModTime()
		if prop != nil {
			prop.NoteActivity() // the transcript moved: an outstanding inject landed
		}
		app.ReopenIfTurnStartedAfterTerminal(id, format, file)
		if reopenOnly {
			if !state.Terminal(id) {
				return
			}
			continue
		}
		ended, reason := session.TurnEnd(format, file)
		if !ended {
			continue
		}
		switch {
		case prop == nil:
			// No pump: conclude on turn-end exactly as before (the flag-absent path).
			app.ConcludeTurnEnd(get(), reason)
		case reason != "":
			// An error turn (a model error, an aborted turn): the pump tolerates a
			// short streak of transient errors by re-injecting, and concludes the
			// session failed itself once the streak cap trips.
			prop.OnTurnError(reason)
		default:
			// A clean turn-end: let the pump decide to re-inject or stop.
			prop.OnTurnEnd()
		}
	}
}

// newPropeller wires the self-propel state machine to its real effects for a
// pi/codex session: the pty writer, the git/session/worker progress
// fingerprint, the --propel-until check, the transcript's final message, the
// human-wait probe, the live-worker count, the conclude path (so `ax wait`
// returns when the loop stops; the pump's reason decides success vs failed),
// and a needs-attention notify for the idle cap.
func newPropeller(get func() string, write func([]byte), dir, group string) *propel.Propeller {
	m := meta.Load(get())
	cfg := propel.ConfigFromSpec(m.Spec)
	var fmtOf string
	c, _ := config.Load()
	for _, h := range c.Harnesses {
		if h.Name == m.Harness {
			fmtOf = h.Format
		}
	}
	axlog.Printf("propel %s: self-propel engaged (max-idle=%d backoff=%s done-check=%t)",
		get(), cfg.MaxIdle, cfg.Backoff, cfg.DoneCmd != "")
	return propel.New(cfg, propel.Deps{
		Write:       write,
		Fingerprint: func() string { return propel.Fingerprint(dir, group, get(), cfg.Watch) },
		DoneCheck:   func() bool { return propel.RunDoneCheck(cfg.DoneCmd, dir) },
		FinalReport: func() string {
			id := get()
			cf, _ := config.Load()
			for _, s := range session.Index(cf) {
				if s.ID == id && s.Host == "" {
					txt, _ := session.LastReportFull(fmtOf, s.File)
					return txt
				}
			}
			return ""
		},
		NeedsHuman:   func() bool { return propel.NeedsHuman(get()) },
		LiveChildren: func() int { return propel.LiveChildren(group, get()) },
		Conclude:     func(reason string) { app.ConcludeTurnEnd(get(), reason) },
		NotifyStuck:  func() { notifyPropelStuck(get()) },
		Log:          func(f string, a ...any) { axlog.Printf(f, a...) },
	})
}

// notifyPropelStuck fires a needs-attention alert when a self-propelled
// coordinator hits the idle cap, so a human is pulled in rather than the pump
// silently giving up.
func notifyPropelStuck(id string) {
	m := meta.Load(id)
	cfg, _ := config.Load()
	notify.Fire(cfg.Notify, notify.Event{
		ID: id, State: notify.NeedsYou, Summary: m.Task, Name: m.Name, Group: m.Group,
	})
}

// heartbeatLoop bumps the liveness mtime on a timer. get returns the id to beat,
// which is empty until an --adopt run discovers its session, so the loop simply
// skips a tick until the id is known.
func heartbeatLoop(get func() string, done <-chan struct{}) {
	t := time.NewTicker(live.Interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if id := get(); id != "" {
				live.Touch(id)
			}
		}
	}
}

// tracker holds the session id a run records liveness and output under. An
// --adopt run fills it once the real id is discovered; until then get() returns
// "" and the writers skip, so no record lands under the placeholder id.
type tracker struct {
	mu sync.RWMutex
	id string
}

func (t *tracker) get() string   { t.mu.RLock(); defer t.mu.RUnlock(); return t.id }
func (t *tracker) set(id string) { t.mu.Lock(); t.id = id; t.mu.Unlock() }

// discoverID finds the session a mint-its-own-id harness (codex, opencode)
// created after launch and adopts it: it records liveness under the real id,
// aliases the placeholder holder endpoint to it via alias (so a later reattach
// by real id finds this same held process), and re-tags the viewer window so
// the picker can locate and focus it. It polls the index (the harness writes
// its transcript a beat after starting) until a session of this harness, in
// this directory, that did not exist at launch shows up, then stops.
func discoverID(harness, axid, command string, before map[string]bool, t *tracker, pid int, procMode bool, alias func(realid string)) {
	cfg, _ := config.Load()
	dir, _ := os.Getwd()
	// The placeholder can never be the discovered session, but the index
	// synthesizes a session for it (its meta sidecar + this run's own live
	// heartbeat) the moment we start, and discovery matching it would
	// self-adopt: the handoff (Save then Remove under the SAME id) deletes the
	// meta and liveness it just wrote, leaving the run untracked.
	before[axid] = true
	deadline := time.Now().Add(90 * time.Second)
	for {
		time.Sleep(1500 * time.Millisecond)
		if id := findNewSession(session.Index(cfg), harness, dir, before); id != "" {
			// Migrate the control-layer identity BEFORE tracking flips to the real
			// id, so a watcher that wakes on the new id never reads a half-moved
			// session: the meta sidecar (group/parent/task/labels, written under
			// the placeholder at launch), any hook state an early failure already
			// recorded, and a durable alias placeholder -> real. The alias is the
			// contract that makes the id printed at launch a working handle for
			// the session's whole life: `ax read/wait/result/send/kill` resolve
			// through it, so a caller never needs to learn the adopted id.
			if m := adoptControlMeta(axid); hasControlMeta(m) {
				meta.Save(id, m)
				meta.Remove(axid)
			}
			if hs, ok := state.HookState(axid); ok {
				state.WriteHook(id, hs)
				state.RemoveHook(axid)
			}
			meta.SaveAlias(axid, id)
			t.set(id)
			live.Start(id, command)
			live.Remove(axid) // liveness handed off from the placeholder
			alias(id)
			mux.New().Retag(id)
			if procMode {
				// Re-track under the real id: record its pid for Interrupt, and
				// symlink its FIFO to the one the harness is already reading (opened
				// at launch under the placeholder id) so Send by real id lands there.
				mux.ProcTrackPID(id, pid)
				os.Symlink(mux.ProcFIFO(axid), mux.ProcFIFO(id))
			}
			axlog.Printf("run %s: adopted %s session %s", axid, harness, id)
			return
		}
		if time.Now().After(deadline) {
			// Discovery failed: fall back to the placeholder id for good, so the
			// run stays trackable (heartbeat, kill, output marks) and its exit
			// still concludes under the id the launch printed, instead of the
			// session silently losing every completion signal.
			t.set(axid)
			axlog.Printf("run %s: no %s session id discovered in time; tracking under the launch id", axid, harness)
			return
		}
	}
}

func adoptControlMeta(axid string) meta.Meta {
	m := meta.Load(axid)
	if m.Group == "" {
		m.Group = app.RunEnv()
	}
	if m.Parent == "" {
		m.Parent = os.Getenv("AX_PARENT")
	}
	if m.Parent != "" && m.Origin == "" {
		m.Origin = "agent"
	}
	if len(m.Labels) == 0 {
		m.Labels = splitEnvLabels(os.Getenv("AX_LABELS"))
	}
	return m
}

func hasControlMeta(m meta.Meta) bool {
	return m.Group != "" || m.Task != "" || m.Name != "" || m.Parent != "" ||
		m.Origin != "" || m.Mode != "" || m.Harness != "" || m.Dir != "" ||
		len(m.Labels) > 0 || m.Spec != nil || m.CloseOnDone || m.KeepLive ||
		!m.KeepUntil.IsZero() || m.Archived || !m.ArchivedAt.IsZero()
}

func splitEnvLabels(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// harnessIDs is the set of session ids of one harness in a snapshot (what
// discovery diffs the launch against).
func harnessIDs(sessions []session.Session, harness string) map[string]bool {
	out := map[string]bool{}
	for _, s := range sessions {
		if s.Harness == harness {
			out[s.ID] = true
		}
	}
	return out
}

// findNewSession returns the newest session of harness in sessions that is not in
// before and whose directory matches dir (when both are known, to disambiguate a
// concurrent launch of the same harness elsewhere), or "" if none has appeared.
func findNewSession(sessions []session.Session, harness, dir string, before map[string]bool) string {
	// Compare RESOLVED paths: os.Getwd() in the adopt wrapper returns the
	// launch dir verbatim from $PWD (e.g. /tmp/hx on macOS), while a harness
	// records the symlink-resolved cwd it canonicalizes to (/private/tmp/hx).
	// A raw string compare then never matches and adoption silently times out,
	// so a codex session in a symlinked dir is never bound to its launch id.
	dir = resolveDir(dir)
	var best session.Session
	for _, s := range sessions {
		if s.Harness != harness || before[s.ID] {
			continue
		}
		if dir != "" && s.Dir != "" && resolveDir(s.Dir) != dir {
			continue
		}
		if best.ID == "" || s.Last.After(best.Last) {
			best = s
		}
	}
	return best.ID
}

// resolveDir returns the symlink-resolved form of d, falling back to d itself
// when it cannot be resolved (a nonexistent or already-canonical path).
func resolveDir(d string) string {
	if d == "" {
		return d
	}
	if r, err := filepath.EvalSymlinks(d); err == nil {
		return r
	}
	return d
}

// aliasSock symlinks the real id's dtach socket to the placeholder one the window
// is actually held under, so reattaching by real id lands on the same held
// process instead of spawning a duplicate. Best-effort, dtach backend only:
// the native holder aliases by listening on both endpoints (no symlink).
func aliasSock(axid, realid string) {
	if hold.Backend() != hold.BackendDtach || !hold.Available() {
		return
	}
	from, to := hold.Sock(realid), hold.Sock(axid)
	if from == to {
		return
	}
	os.Remove(from)
	os.Symlink(to, from)
}

// newArgs parses `ax new [--with-args] [harness] [flags...]` into the arguments
// New wants: an optional preselected harness and, when flags follow, an explicit
// args string. --with-args applies the chosen harness's configured flags (what a
// remote `ax new` runs for the picker's C). Binding a favorite is then just a
// tmux key running e.g. `ax new claude --dangerously-skip-permissions`.
func newArgs(args []string) (name string, withArgs bool, explicit *string) {
	if len(args) > 0 && args[0] == "--with-args" {
		withArgs = true
		args = args[1:]
	}
	if len(args) == 0 {
		return "", withArgs, nil
	}
	if len(args) > 1 {
		s := strings.Join(args[1:], " ")
		return args[0], withArgs, &s
	}
	return args[0], withArgs, nil
}

func usage() {
	fmt.Print(`ax: agentswitch session switcher and control layer

 examples
  ax                             open the picker and resume a session
  ax claude "fix the flaky test" run a task in a tracked background window
  ax "fix the flaky test"        same, when default_harness is set in config
  ax new                         start a fresh interactive session

 Full manual and recipes: https://agentswitch.org

 sessions
  ax [pick]              open the picker (resume a past session)
  ax new [--with-args] [harness [flags...]]
                         start a fresh session interactively (pick dir + harness);
                         --with-args applies the harness's configured args
  ax list [--run R] [--json] [--federated|--hosts]
                         print local indexed sessions; --federated adds hosts
                         configured on this machine; --json emits the wire report
                         --all includes archived, --archived shows only archived
  ax attach <id> [--args FLAGS]
                         (re)attach a held session in this window (ctrl-a then
                         d, or closing the window, detaches; ctrl-a then a
                         detaches and reopens the picker; rebind via
                         detach_prefix / detach_key / menu_key)
  ax preview <id>        print a session's preview (served to remote viewers)
  ax search <query>      print ids of sessions whose transcript matches; --json
                         returns ranked matches with metadata and snippets
  ax kill [--host H] <id>... | --run R
                         stop sessions; --run cascade-kills a whole run
  ax archive [--host H] [--force] <id>...
                         hide sessions from default views, never delete data
  ax unarchive [--host H] <id>...
                         restore archived sessions to default views
  ax prune [--run R | --older-than D | --all] [--dry-run] [--host H] [--reap-workers]
                         archive concluded ephemeral workers and crashed ghosts;
                         --reap-workers also closes/kills concluded resident workers
  ax move --tag k=v | --run R | <id>... [--to NAME]
                         move sessions' windows into their own tmux session
  ax log                 print the diagnostic log (session launches, errors)
  ax models update       refresh model prices/context from models.dev

 control layer (drive sessions from a session)
  ax <harness> "TASK"    run a task; prints the session id and run (or just
  ax "TASK"              ax "TASK" when default_harness is set). Runs watched
                         (interactive) in a tracked window by default and concludes
                         into a "done" state when the task finishes. --wait blocks
                         the caller on the job; --unattended runs it with no human;
                         both still run the interactive form under the holder, so the
                         job is attachable and shows its live TUI when you drop in.
                         --headless is the ONLY opt-in to the screenless claude -p
                         form (no attachable TUI), for a pure scripted job.
     [--task-file PATH|-] [--behavior PATH] [--behavior-text TEXT] [--model M] [--effort E]
     [--run R] [--group R] [--name N] [--parent P] [--label k=v]... [--host H]
     [--max-cost N] [--max-tokens N] [--max-workers N] [--max-depth N] [--timeout D]
     [--write GLOB]... [--no-write] [--no-subagents] [--fence best-effort]
     [--accept ./check.sh] [--wait] [--unattended] [--interactive] [--attach] [--headless] [--json] [--dir D] [-- FLAGS]
     [--close-on-done] [--clean-env] [--env KEY=VAL] [--auth subscription|api|env:VAR] [--api]
     [--keep-live] [--keep-live-for D]
	     [--self-propel [--propel-prompt P] [--propel-until CMD] [--max-idle-turns N]
	      [--propel-backoff D] [--propel-watch PATH]]
	                         --self-propel (pi/codex only) is the outer loop: when the
	                         session ends a turn but its task is not done, ax re-invokes
	                         it so a one-burst local model keeps grinding. --propel-prompt
	                         replaces the generic built-in continue-prompt. Stops on a done
	                         sentinel (PROJECT-COMPLETE), a --propel-until check that exits
	                         0, a human wait, or --max-idle-turns (default 8) no-progress
	                         turns; progress is git state, the run's sessions and live
	                         workers, plus an optional --propel-watch file's mtime. It
	                         never gives up while the session's own workers are still
	                         running, and tolerates a short streak of transient error
	                         turns. Implies --keep-live. Deprecated aliases still parse:
	                         --done-check for --propel-until, and --propel-max-idle for
	                         --max-idle-turns.
	                         --write GLOB (repeatable) launches under the capability fence:
	                         file writes are allowed only where a glob matches, mutating
	                         shell is denied. --no-write allows no writes at all;
	                         --no-subagents also denies in-process subagents. A task-carrying
	                         watched worker concludes into a "done" state (alerts via notify)
	                         when its task finishes and is reaped after retention.reap_after;
	                         --keep-live opts out, --close-on-done ends the session immediately.
                         --clean-env starts the child from a minimal environment; --env
                         sets a variable for it; --auth picks the auth source (default
                         subscription; env:VAR forces a specific key without exposing it).
                         --api is the deprecated alias for --auth api. --effort forwards
                         the harness reasoning-effort setting when the harness supports it.
                         --group/AX_GROUP still work as deprecated aliases for --run/AX_RUN.
  ax restart <id> [--fresh]
                         rebuild a session from its persisted launch spec (same task,
                         model, fences, env, and auth), pinned back into its run;
                         --fresh also cleans up its socket and process files
  ax continue <id> "TASK" [launch flags]
                         resume a session's context with a new task, tracked and
                         scriptable: the reuse primitive between ax send (needs a
                         live window) and a cold launch. Watched by default; --wait
                         runs it as a job. A harness with no resume-with-input form
                         degrades with a message
  ax read [--host H] <id>|--run R [--hosts|--federated] [--since N] [--limit N] [--format json|text]
                         print turns; --follow streams turn/waiting/exit events
                         (--active, --from-now, --with-content, --timeout D, --events LIST,
                         --exclude ID, --exclude-self). --hosts/--federated requires --run,
                         keeps the default local-only behavior opt-in, emits JSON rows/events
                         with host identity, and fans out to configured hosts
  ax wait [--host H] <id>... [--timeout D] [--all|--any]
                         block until sessions reach a terminal state (done/failed);
                         exit 0 on success, non-zero on failure, 124 on timeout
  ax result [--host H] <id> [--json]
                         print a concluded session's final report (its last
                         assistant message) plus outcome and exit, the interactive
                         equivalent of the final answer from a headless claude -p
  ax send [--host H] <id> [TEXT]
                         type input into a running session (--stdin, --interrupt, --no-enter)
  ax tag <id> ...        set metadata (--name --run --group --parent --origin
                         --task --outcome --add-label --rm-label)
  ax check               run this run's --accept check and print its output + status
  ax ask [--default TEXT] <question>
                         block for a human answer (called by a session)
  ax reply <id> <answer> answer a blocked ax ask
  ax runs [--follow]     list run records (--json)
  ax metrics             session/run cost, tokens, duration, outcome counts
                         (--json for scripting, --prom for a textfile export)
  ax host register --name N --transport T [--ax PATH]
  ax host deregister --name N
  ax hook install <harness>   report state authoritatively via the harness's hooks
  ax version             print this ax build's version

 config propagation (push the portable profile to remote hosts)
  ax config export-profile    print this box's portable profile (harness
                         templates + UI/behavior) as TOML; local paths/secrets
                         are excluded and a matching value is refused
  ax config apply-profile     read a profile on STDIN and merge it into the
                         local config (--dry-run just shows the diff); local
                         fields are preserved, an atomic write with a backup
  ax config sync [--host N | --all] [--dry-run] [--yes]
                         push this box's profile over each host's ssh transport
                         into its apply-profile, overwriting the remote's
                         profile while preserving its local settings
  ax config status [--host N | --all] [--json]
                         show remote ax/wire versions and profile drift
  ax config rollback [--host N] [--yes]
                         restore the newest config backup locally or on a host

 extensions
  ax <verb> ...          any ax-<verb> executable on PATH runs as ax <verb>
`)
}
