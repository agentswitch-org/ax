package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
	hostreg "github.com/agentswitch-org/ax/internal/hosts"
)

// shellWarned records which hosts have already emitted the missing-shell
// warning, so warnMissingShell fires at most once per host per process.
var shellWarned sync.Map

// shellWarnOut is where warnMissingShell writes; nil means os.Stderr, resolved
// at write time. shellWarnMu guards it: the federated fan-out warns from per-host
// goroutines while bufferShellWarnings swaps the sink on the picker goroutine.
var (
	shellWarnMu  sync.Mutex
	shellWarnOut io.Writer
)

// bufferShellWarnings redirects missing-shell warnings into a buffer and
// returns a flush that restores the previous sink and writes anything collected
// to os.Stderr. The picker wraps its alt-screen lifetime in this: the federated
// fan-out shells out to each host while the frame is live, and a warning
// printed to stderr then lands inside the rendered frame and corrupts the
// display. Buffering keeps the warning out of the frame without losing it (it
// flushes once the terminal is back on the normal screen). A fan-out goroutine
// that warns after flush writes straight to stderr, which is safe for the same
// reason.
func bufferShellWarnings() (flush func()) {
	var buf bytes.Buffer
	shellWarnMu.Lock()
	prev := shellWarnOut
	shellWarnOut = &buf
	shellWarnMu.Unlock()
	return func() {
		shellWarnMu.Lock()
		shellWarnOut = prev
		out := buf.Bytes()
		shellWarnMu.Unlock()
		if len(out) > 0 {
			os.Stderr.Write(out)
		}
	}
}

// warnMissingShell warns, once per host, that an ssh host with no shell set
// falls back to POSIX quoting. A POSIX-quoted argv mis-quotes on a
// PowerShell/Windows remote (pwsh does not honor the POSIX embedded-quote
// escape), which has enabled a real command injection, so the operator is
// nudged to set shell = "pwsh". It only fires for an ssh transport that still
// quotes: a raw_argv transport passes argv verbatim (no quoting to get wrong),
// a non-ssh transport is not the ambiguous case, and a host with any shell set
// is already explicit. It is called from transportArgv, so a host warns only
// when it is actually used to shell out, not on every unrelated command.
func warnMissingShell(h config.Host) {
	if h.Shell != "" || h.RawArgv {
		return
	}
	fields := strings.Fields(h.Transport)
	if len(fields) == 0 || filepath.Base(fields[0]) != "ssh" {
		return
	}
	if _, seen := shellWarned.LoadOrStore(h.Name, struct{}{}); seen {
		return
	}
	shellWarnMu.Lock()
	defer shellWarnMu.Unlock()
	out := io.Writer(os.Stderr)
	if shellWarnOut != nil {
		out = shellWarnOut
	}
	fmt.Fprintf(out, "ax: host %q: no shell set, defaulting to POSIX quoting. "+
		"If this host runs PowerShell/Windows, set shell = \"pwsh\" in its [[host]] block to avoid mis-quoting.\n", h.Name)
}

// remoteVerbTimeout bounds a one-shot remote verb (result, tag, kill, send).
// It is deliberately larger than fanoutTimeout (3s), which only guards a single
// `ax list --json` poll: a one-shot verb runs a real ax command on the host and
// remoteTransport injects ConnectTimeout=8, so a 3s ceiling would trip before
// the ssh connection could even fail fast. Streaming verbs (read --follow, wait)
// are unbounded — an orchestrating client must be able to block for a remote
// worker's whole run.
const remoteVerbTimeout = 20 * time.Second

// dehost pulls a host target out of a verb's args: an explicit `--host NAME`
// (consuming its value) and/or a `host/id` qualifier on the FIRST bare positional
// id. It returns the host ("" = local) and the args with the routing stripped, so
// the id is bare and --host is gone, ready to forward verbatim to the remote ax.
//
// It only ever splits the first bare-id positional, and --host wins the host name
// when both forms are present, so the two resolve consistently. dehost must run
// AFTER GroupArg/`--run` parsing at each call site: a value-taking flag left in
// args (e.g. `--run R` with no id) must not have its value mistaken for the id.
func dehost(args []string) (host string, rest []string) {
	rest = make([]string, 0, len(args))
	firstBare := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--host" {
			if i+1 < len(args) {
				host = args[i+1]
				i++
			}
			continue
		}
		if firstBare && !strings.HasPrefix(a, "-") {
			firstBare = false
			if h, id, ok := strings.Cut(a, "/"); ok {
				if host == "" {
					host = h
				}
				rest = append(rest, id)
				continue
			}
		}
		rest = append(rest, a)
	}
	return host, rest
}

func findHost(name string) (config.Host, bool) {
	cfg, _ := config.Load()
	for _, h := range hostreg.Merge(cfg.Hosts) {
		if h.Name == name {
			return h, true
		}
	}
	return config.Host{}, false
}

// lookupHost resolves a configured or self-registered host by name (config.Load
// + hostreg.Merge, matching Send's old sendRemote), or exits 1 with
// "ax: unknown host %q" so a typo'd host does not silently run locally.
func lookupHost(name string) config.Host {
	if h, ok := findHost(name); ok {
		return h
	}
	fmt.Fprintf(os.Stderr, "ax: unknown host %q\n", name)
	os.Exit(1)
	return config.Host{}
}

// remoteTransport returns h with its transport adjusted for a non-pty remote
// verb. A pty (`ssh -t`) puts the remote in cooked mode, which mangles piped
// stdin (CR/LF translation, control chars interpreted) and CR-pads captured
// stdout, so `-t`/`-tt` are stripped for every remoteVerb call (attach keeps its
// own path and is untouched). For a one-shot ssh verb (streaming=false), also
// inject `-o BatchMode=yes` and `-o ConnectTimeout=8` when absent, so a missing
// key fails fast instead of blocking a headless caller on a password prompt.
// raw_argv transports (kubectl exec, docker exec) carry no ssh options and are
// left alone.
func remoteTransport(h config.Host, streaming bool) config.Host {
	fields := strings.Fields(h.Transport)
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		if f == "-t" || f == "-tt" {
			continue // no pty: it corrupts piped stdin and captured stdout
		}
		kept = append(kept, f)
	}
	if !streaming && !h.RawArgv && len(kept) > 0 && filepath.Base(kept[0]) == "ssh" {
		kept = injectSSHOpts(kept)
	}
	h.Transport = strings.Join(kept, " ")
	return h
}

// injectSSHOpts inserts `-o BatchMode=yes` / `-o ConnectTimeout=8` right after
// the ssh program token, skipping either option already spelled out in the
// transport, so a user-set ConnectTimeout or BatchMode is not doubled.
func injectSSHOpts(fields []string) []string {
	joined := strings.Join(fields, " ")
	var opts []string
	if !strings.Contains(joined, "BatchMode=") {
		opts = append(opts, "-o", "BatchMode=yes")
	}
	if !strings.Contains(joined, "ConnectTimeout=") {
		opts = append(opts, "-o", "ConnectTimeout=8")
	}
	if len(opts) == 0 {
		return fields
	}
	out := make([]string, 0, len(fields)+len(opts))
	out = append(out, fields[0])
	out = append(out, opts...)
	return append(out, fields[1:]...)
}

// remoteArgv builds the (prog, argv) that reruns `<verb> <args...>` on h over its
// transport, pty-stripped (and, one-shot, fail-fast) via remoteTransport and
// quoted per the host's shell via transportArgv. Pure and side-effect free so the
// argv can be asserted in a test without executing anything.
func remoteArgv(h config.Host, verb string, args []string, streaming bool) (string, []string) {
	axArgs := append([]string{verb}, args...)
	return transportArgv(remoteTransport(h, streaming), axArgs...)
}

// remoteVerb reruns `<verb> <args...>` on host over its transport with stdin,
// stdout, and stderr wired straight through, and PROPAGATES the remote command's
// exit code (so `ax wait host/id` returns the remote code and an orchestrating
// client's accept-gate can tell success from failure). Exit code 255 is reserved: ssh
// returns 255 on a connection failure and ax never exits 255, so a 255 is
// reported as a distinct, retryable transport error rather than collapsed into a
// real remote failure. streaming=false wraps the run in a context timeout;
// streaming=true (read --follow, wait) runs unbounded.
func (a App) remoteVerb(verb, host string, args []string, streaming bool) {
	code := remoteVerbExitCode(context.Background(), verb, host, args, streaming)
	if code != 0 {
		os.Exit(code)
	}
}

type remoteCommandFunc func(ctx context.Context, prog string, argv []string) error

var runRemoteCommand remoteCommandFunc = func(ctx context.Context, prog string, argv []string) error {
	c := exec.CommandContext(ctx, prog, argv...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func remoteVerbExitCode(parent context.Context, verb, host string, args []string, streaming bool) int {
	h, ok := findHost(host)
	if !ok {
		fmt.Fprintf(os.Stderr, "ax: unknown host %q\n", host)
		return 1
	}
	prog, argv := remoteArgv(h, verb, args, streaming)

	ctx := parent
	if ctx == nil {
		ctx = context.Background()
	}
	cancel := context.CancelFunc(func() {})
	if !streaming {
		ctx, cancel = context.WithTimeout(ctx, remoteVerbTimeout)
	}
	defer cancel()

	err := runRemoteCommand(ctx, prog, argv)
	return remoteCommandExitCode(ctx, host, err)
}

func remoteCommandExitCode(ctx context.Context, host string, err error) int {
	if err == nil {
		return 0
	}
	if ctx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(os.Stderr, "ax: transport to %s timed out\n", host)
		return 255
	}
	if ctx.Err() == context.Canceled {
		return 124
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch code := ee.ExitCode(); {
		case code == 255:
			fmt.Fprintf(os.Stderr, "ax: transport to %s failed\n", host)
			return 255
		case code >= 0:
			return code // propagate the remote command's own exit code
		}
	}
	// Transport never started (ssh not found, killed by signal): treat as a
	// transport error, not a remote failure.
	fmt.Fprintf(os.Stderr, "ax: transport to %s failed: %v\n", host, err)
	return 255
}
