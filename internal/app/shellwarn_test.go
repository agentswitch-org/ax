package app

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
)

// An ssh host with no shell set must warn (once) that it defaults to POSIX
// quoting, since that mis-quotes on a PowerShell/Windows remote.
func TestTransportArgvWarnsSshHostNoShell(t *testing.T) {
	h := config.Host{Name: "win-noshell", Transport: "ssh win01"}
	stderr := captureStderr(t, func() {
		transportArgv(h, "list", "--json")
	})
	if !strings.Contains(stderr, `"win-noshell"`) ||
		!strings.Contains(stderr, "no shell set") ||
		!strings.Contains(stderr, `pwsh`) {
		t.Fatalf("expected a missing-shell warning naming the host, got: %q", stderr)
	}
}

// The warning fires at most once per host per process, even across many uses.
func TestTransportArgvWarnsOncePerHost(t *testing.T) {
	h := config.Host{Name: "win-once", Transport: "ssh win01"}
	first := captureStderr(t, func() { transportArgv(h, "list") })
	second := captureStderr(t, func() { transportArgv(h, "list") })
	if first == "" {
		t.Fatalf("expected a warning on first use, got none")
	}
	if second != "" {
		t.Fatalf("expected no repeat warning on second use, got: %q", second)
	}
}

// A host that sets shell = "pwsh" is already explicit and must not warn.
func TestTransportArgvNoWarnShellSet(t *testing.T) {
	h := config.Host{Name: "win-pwsh", Transport: "ssh win01", Shell: "pwsh"}
	stderr := captureStderr(t, func() {
		transportArgv(h, "list")
	})
	if stderr != "" {
		t.Fatalf("host with shell set must not warn, got: %q", stderr)
	}
}

// While the picker owns the alt-screen (bufferShellWarnings active), a
// missing-shell warning must not reach stderr, where it would land inside the
// rendered frame. It is buffered and flushed to stderr after the picker exits.
func TestShellWarnBufferedDuringPicker(t *testing.T) {
	h := config.Host{Name: "win-buffered", Transport: "ssh win01"}
	flush := bufferShellWarnings()
	during := captureStderr(t, func() { transportArgv(h, "list", "--json") })
	if during != "" {
		t.Fatalf("warning must not reach stderr while the picker is up, got: %q", during)
	}
	after := captureStderr(t, flush)
	if !strings.Contains(after, `"win-buffered"`) || !strings.Contains(after, "no shell set") {
		t.Fatalf("expected the buffered warning on flush, got: %q", after)
	}
}

// A flush with nothing buffered writes nothing.
func TestShellWarnFlushEmpty(t *testing.T) {
	flush := bufferShellWarnings()
	out := captureStderr(t, flush)
	if out != "" {
		t.Fatalf("empty flush must write nothing, got: %q", out)
	}
}

// After flush the sink is restored: a warning fired later (a straggling fan-out
// goroutine, or any post-picker shell-out) goes straight to stderr.
func TestShellWarnRestoredAfterFlush(t *testing.T) {
	bufferShellWarnings()()
	h := config.Host{Name: "win-restored", Transport: "ssh win01"}
	stderr := captureStderr(t, func() { transportArgv(h, "list") })
	if !strings.Contains(stderr, "no shell set") {
		t.Fatalf("expected the warning on stderr after flush, got: %q", stderr)
	}
}

// A non-ssh (raw_argv) transport passes argv verbatim, so there is no quoting
// to get wrong and no warning. A local/kubectl transport likewise must not warn.
func TestTransportArgvNoWarnNonSsh(t *testing.T) {
	raw := config.Host{Name: "k8s-raw", Transport: "kubectl exec -n ns pod --", RawArgv: true}
	stderr := captureStderr(t, func() {
		transportArgv(raw, "list")
	})
	if stderr != "" {
		t.Fatalf("raw_argv transport must not warn, got: %q", stderr)
	}

	nonSsh := config.Host{Name: "docker-noshell", Transport: "docker exec box"}
	stderr = captureStderr(t, func() {
		transportArgv(nonSsh, "list")
	})
	if stderr != "" {
		t.Fatalf("non-ssh transport must not warn, got: %q", stderr)
	}
}
