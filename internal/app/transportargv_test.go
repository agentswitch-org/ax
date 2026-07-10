package app

import (
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
)

// transportArgv joins argv into one string the remote shell re-parses, so it
// must quote each value for THAT shell. A pwsh remote does not honor the POSIX
// embedded-quote escape, so a crafted value could otherwise inject commands.
func TestTransportArgvQuotesPerHostShell(t *testing.T) {
	// A forwarded label carrying an embedded quote plus a command payload: the
	// classic injection vector on a remote host.
	inject := `x="';calc;#"`

	t.Run("pwsh host uses doubled-quote escaping", func(t *testing.T) {
		h := config.Host{Name: "win01", Transport: "ssh win01", Shell: "pwsh"}
		_, argv := transportArgv(h, "send", inject)
		got := argv[len(argv)-1] // the quoted inject arg is last
		// POSIX '\'' escaping would flip quote parity in pwsh and expose ;calc.
		// The pwsh form doubles the quote, keeping ;calc inside the literal.
		if strings.Contains(got, `'\''`) {
			t.Fatalf("pwsh host quoted with POSIX escape %q (injection vector)", got)
		}
		if want := `''';calc;#"'`; !strings.HasSuffix(got, `;calc;#"'`) || !strings.HasPrefix(got, `'x="''`) {
			t.Fatalf("pwsh host quoted %q, want doubled-quote form ending like %q", got, want)
		}
		// The payload must not sit outside a quoted region as a bare statement.
		if idx := strings.Index(got, ";calc"); idx == 0 {
			t.Fatalf("pwsh host left ;calc unquoted in %q", got)
		}
	})

	t.Run("default host uses POSIX escaping", func(t *testing.T) {
		h := config.Host{Name: "vm", Transport: "ssh vm"} // Shell empty => posix
		_, argv := transportArgv(h, "send", inject)
		got := argv[len(argv)-1]
		if !strings.Contains(got, `'\''`) {
			t.Fatalf("posix host should use '\\'' escaping, got %q", got)
		}
		// pwsh doubled-quote form must NOT appear on a posix host.
		if strings.HasPrefix(got, `'x="''`) {
			t.Fatalf("posix host emitted pwsh doubled-quote form %q", got)
		}
	})

	t.Run("raw_argv host passes argv verbatim", func(t *testing.T) {
		h := config.Host{Name: "pod", Transport: "kubectl exec pod --", RawArgv: true}
		_, argv := transportArgv(h, "send", inject)
		got := argv[len(argv)-1]
		if got != inject {
			t.Fatalf("raw_argv host altered arg: got %q, want verbatim %q", got, inject)
		}
	})

	// A pwsh host still gets raw (verbatim) args when raw_argv is set: raw wins.
	t.Run("raw_argv overrides pwsh shell", func(t *testing.T) {
		h := config.Host{Name: "x", Transport: "t --", RawArgv: true, Shell: "pwsh"}
		_, argv := transportArgv(h, "send", inject)
		if got := argv[len(argv)-1]; got != inject {
			t.Fatalf("raw_argv should win over shell=pwsh: got %q, want %q", got, inject)
		}
	})
}
