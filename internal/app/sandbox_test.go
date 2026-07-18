package app

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
)

// sandboxCmd wraps the final command in `nono run` when asked, resolves the
// profile harness > config > derived, hard-fails an explicit --sandbox when
// nono is missing, and stays a no-op when off.
func TestSandboxCmd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sandbox is POSIX-only")
	}
	defer func(orig func() (string, error)) { lookNono = orig }(lookNono)
	lookNono = func() (string, error) { return "/usr/local/bin/nono", nil }

	h := config.Harness{Name: "claude", Format: "claude"}
	cfg := config.Config{}

	// Default: off, command untouched.
	out, err := sandboxCmd(cfg, h, launchOpts{}, "claude -p hi")
	if err != nil || out != "claude -p hi" {
		t.Fatalf("default = %q, %v; want untouched", out, err)
	}

	// --sandbox wraps with the derived profile.
	out, err = sandboxCmd(cfg, h, launchOpts{sandbox: "on"}, "claude -p hi")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"nono", "run --profile", "nolabs-ai/claude", "sh -c", "claude -p hi"} {
		if !strings.Contains(out, want) {
			t.Fatalf("wrapped = %q, missing %q", out, want)
		}
	}

	// Profile precedence: harness sandbox_profile > [sandbox].profile > derived.
	cfgP := config.Config{Sandbox: config.Sandbox{Profile: "me/base"}}
	out, _ = sandboxCmd(cfgP, h, launchOpts{sandbox: "on"}, "c")
	if !strings.Contains(out, "me/base") {
		t.Fatalf("config profile not used: %q", out)
	}
	hP := h
	hP.SandboxProfile = "me/claude-tuned"
	out, _ = sandboxCmd(cfgP, hP, launchOpts{sandbox: "on"}, "c")
	if !strings.Contains(out, "me/claude-tuned") {
		t.Fatalf("harness profile not used: %q", out)
	}

	// [sandbox] backend = "nono" sandboxes by default; --no-sandbox opts out.
	cfgB := config.Config{Sandbox: config.Sandbox{Backend: "nono"}}
	out, _ = sandboxCmd(cfgB, h, launchOpts{}, "c")
	if !strings.Contains(out, "nono") {
		t.Fatalf("backend=nono did not sandbox: %q", out)
	}
	out, _ = sandboxCmd(cfgB, h, launchOpts{sandbox: "off"}, "c")
	if out != "c" {
		t.Fatalf("--no-sandbox did not opt out: %q", out)
	}

	// Explicit --sandbox with no nono on PATH is a hard error; the config
	// backend only warns and degrades.
	lookNono = func() (string, error) { return "", errors.New("not found") }
	if _, err := sandboxCmd(cfg, h, launchOpts{sandbox: "on"}, "c"); err == nil {
		t.Fatal("--sandbox without nono must fail")
	}
	if out, err := sandboxCmd(cfgB, h, launchOpts{}, "c"); err != nil || out != "c" {
		t.Fatalf("backend degrade = %q, %v; want unsandboxed no error", out, err)
	}
}
