package app

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
)

// remoteLaunchArgv reruns a launch on a host. It must forward the run/label/
// model/behavior/effort/fence/api/dir flags and the harness flags after --, feed
// the task over stdin via --task-file - (never as a quoted arg), ask for --json
// so the id is machine-parseable, drop --host (we are already on the host), and
// add --headless only when the host forces it.
func TestRemoteLaunchArgv(t *testing.T) {
	o := launchOpts{
		task:     "do the thing",
		host:     "win01", // must NOT leak into the remote argv
		group:    "R",
		parent:   "coord",
		labels:   []string{"role=worker", "project=blog"},
		model:    "sonnet",
		behavior: "be terse",
		effort:   "high",
		api:      true,
		dir:      "/srv/app",
		hflags:   []string{"--verbose"},
	}
	o.fen.writeGlobs = []string{"src/**"}
	o.fen.noSubagents = true
	o.fen.maxWorkers = 3
	o.fen.maxDepth = 2

	args := remoteLaunchArgv("claude", o, false)
	joined := strings.Join(args, " ")

	if args[0] != "claude" {
		t.Fatalf("verb = %q, want claude", args[0])
	}
	// --task-file - and --json are mandatory.
	if !hasPair(args, "--task-file", "-") {
		t.Errorf("missing --task-file -: %q", joined)
	}
	if !hasFlag(args, "--json") {
		t.Errorf("missing --json: %q", joined)
	}
	// --host must never be forwarded.
	if hasFlag(args, "--host") || strings.Contains(joined, "win01") {
		t.Errorf("--host leaked into remote argv: %q", joined)
	}
	// The task text must go over stdin, never as an arg.
	if strings.Contains(joined, "do the thing") {
		t.Errorf("task text leaked into argv (must go over stdin): %q", joined)
	}
	// Forwarded flags.
	for _, want := range [][2]string{
		{"--run", "R"},
		{"--parent", "coord"},
		{"--model", "sonnet"},
		{"--behavior", "be terse"},
		{"--effort", "high"},
		{"--write", "src/**"},
		{"--max-workers", "3"},
		{"--max-depth", "2"},
		{"--dir", "/srv/app"},
	} {
		if !hasPair(args, want[0], want[1]) {
			t.Errorf("missing %s %s: %q", want[0], want[1], joined)
		}
	}
	for _, l := range o.labels {
		if !hasPair(args, "--label", l) {
			t.Errorf("missing --label %s: %q", l, joined)
		}
	}
	if !hasFlag(args, "--no-subagents") {
		t.Errorf("missing --no-subagents: %q", joined)
	}
	if !hasFlag(args, "--api") {
		t.Errorf("missing --api: %q", joined)
	}
	// Harness flags after --.
	if !strings.HasSuffix(joined, "-- --verbose") {
		t.Errorf("harness flags not forwarded after --: %q", joined)
	}
	// headless=false: no --headless.
	if hasFlag(args, "--headless") {
		t.Errorf("--headless present when host does not force it: %q", joined)
	}
}

// --headless is added iff the host forces headless.
func TestRemoteLaunchArgvHeadless(t *testing.T) {
	o := launchOpts{task: "x", group: "R"}
	if hasFlag(remoteLaunchArgv("claude", o, false), "--headless") {
		t.Errorf("--headless added when host is not headless")
	}
	if !hasFlag(remoteLaunchArgv("claude", o, true), "--headless") {
		t.Errorf("--headless not added when host forces headless")
	}
}

func TestRemoteLaunchArgvBehaviorText(t *testing.T) {
	o := launchOpts{task: "x", behaviorText: "inline\nbehavior"}
	args := remoteLaunchArgv("claude", o, false)
	if !hasPair(args, "--behavior-text", "inline\nbehavior") {
		t.Fatalf("missing --behavior-text: %q", strings.Join(args, " "))
	}
	if hasFlag(args, "--behavior") {
		t.Fatalf("--behavior must not be emitted for inline behavior text: %q", strings.Join(args, " "))
	}
}

// A pwsh host quotes the forwarded flags with PowerShell's doubled-quote form,
// not the POSIX '\” escape, when the argv is transported.
func TestRemoteLaunchArgvPwshQuoting(t *testing.T) {
	o := launchOpts{task: "x", labels: []string{"role=o'brien"}}
	h := config.Host{Name: "win01", Transport: "ssh -t win01", Shell: "pwsh"}
	_, argv := transportArgv(remoteTransport(h, false), remoteLaunchArgv("claude", o, true)...)
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, `'\''`) {
		t.Fatalf("pwsh host used POSIX escaping: %q", joined)
	}
	if !strings.Contains(joined, "o''brien") {
		t.Fatalf("pwsh quoting = %q, want doubled-quote form o''brien", joined)
	}
	// -t must be stripped for a clean id capture.
	if strings.Contains(joined, " -t ") || strings.HasPrefix(joined, "-t ") {
		t.Fatalf("pty -t not stripped for capture: %q", joined)
	}
}

// parseRemoteLaunch extracts the id and group from the remote --json line even
// when the pty surrounds it with noise, and errors cleanly on none.
func TestParseRemoteLaunch(t *testing.T) {
	t.Run("clean line", func(t *testing.T) {
		id, group, err := parseRemoteLaunch(`{"id":"abc-123","group":"R"}`)
		if err != nil || id != "abc-123" || group != "R" {
			t.Fatalf("got id=%q group=%q err=%v", id, group, err)
		}
	})
	t.Run("surrounding noise and CR padding", func(t *testing.T) {
		blob := "Warning: Permanently added 'win01'\r\ntrust prompt noise\r\n" +
			"  {\"id\":\"u-1\",\"group\":\"g\"}  \r\nbye\r\n"
		id, group, err := parseRemoteLaunch(blob)
		if err != nil || id != "u-1" || group != "g" {
			t.Fatalf("got id=%q group=%q err=%v", id, group, err)
		}
	})
	t.Run("no id errors", func(t *testing.T) {
		if _, _, err := parseRemoteLaunch("no json here\r\nstill none\r\n"); err == nil {
			t.Fatal("want error on output with no launch id")
		}
	})
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func hasPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}
