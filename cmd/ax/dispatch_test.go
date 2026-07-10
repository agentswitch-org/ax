package main

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
)

// cfgWithDefault returns a minimal config with default_harness set.
func cfgWithDefault(harness string) config.Config {
	return config.Config{
		DefaultHarness: harness,
		Harnesses: []config.Harness{
			{Name: "claude"},
			{Name: "pi"},
		},
	}
}

// cfgNoDefault returns a minimal config with no default_harness.
func cfgNoDefault() config.Config {
	return config.Config{
		Harnesses: []config.Harness{
			{Name: "claude"},
			{Name: "pi"},
		},
	}
}

func TestClassifyCmd_NoArgs(t *testing.T) {
	action, name, args := classifyCmd(nil, cfgNoDefault())
	if action != actionPick || name != "" || len(args) != 0 {
		t.Fatalf("no args: want pick/''/[], got %v/%q/%v", action, name, args)
	}
}

func TestClassifyCmd_FlagOnlyArgs(t *testing.T) {
	// First arg starting with - should still be picker.
	action, _, _ := classifyCmd([]string{"--model", "opus"}, cfgNoDefault())
	if action != actionPick {
		t.Fatalf("flag-first args: want pick, got %v", action)
	}
}

func TestClassifyCmd_KnownVerb(t *testing.T) {
	action, name, args := classifyCmd([]string{"list", "--json"}, cfgNoDefault())
	if action != actionVerb || name != "list" {
		t.Fatalf("list verb: want verb/list, got %v/%q", action, name)
	}
	if len(args) != 1 || args[0] != "--json" {
		t.Fatalf("list verb: want [--json], got %v", args)
	}
}

func TestClassifyCmd_KnownHarness(t *testing.T) {
	action, name, args := classifyCmd([]string{"claude", "fix the flaky test"}, cfgNoDefault())
	if action != actionHarness || name != "claude" {
		t.Fatalf("known harness: want harness/claude, got %v/%q", action, name)
	}
	if len(args) != 1 || args[0] != "fix the flaky test" {
		t.Fatalf("known harness: want [fix the flaky test], got %v", args)
	}
}

func TestClassifyCmd_DefaultHarness_MultiWordPrompt(t *testing.T) {
	cfg := cfgWithDefault("claude")
	action, name, args := classifyCmd([]string{"some multi word prompt"}, cfg)
	if action != actionDefault || name != "claude" {
		t.Fatalf("default harness: want default/claude, got %v/%q", action, name)
	}
	if len(args) != 1 || args[0] != "some multi word prompt" {
		t.Fatalf("default harness: want [some multi word prompt], got %v", args)
	}
}

func TestClassifyCmd_DefaultHarness_WithFlags(t *testing.T) {
	cfg := cfgWithDefault("claude")
	// ax "fix it" --model opus  ->  Launch("claude", ["fix it", "--model", "opus"])
	action, name, args := classifyCmd([]string{"fix it", "--model", "opus"}, cfg)
	if action != actionDefault || name != "claude" {
		t.Fatalf("default+flags: want default/claude, got %v/%q", action, name)
	}
	want := []string{"fix it", "--model", "opus"}
	if len(args) != len(want) {
		t.Fatalf("default+flags: want %v, got %v", want, args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("default+flags: args[%d] want %q, got %q", i, want[i], args[i])
		}
	}
}

func TestClassifyCmd_VerbTakesPrecedenceOverDefault(t *testing.T) {
	// "list" is a known verb; even with default_harness set it must route as verb.
	cfg := cfgWithDefault("claude")
	action, name, _ := classifyCmd([]string{"list"}, cfg)
	if action != actionVerb || name != "list" {
		t.Fatalf("verb precedence: want verb/list, got %v/%q", action, name)
	}
}

func TestClassifyCmd_HarnessTakesPrecedenceOverDefault(t *testing.T) {
	// "pi" is a known harness; even with default_harness set it must route as harness.
	cfg := cfgWithDefault("claude")
	action, name, args := classifyCmd([]string{"pi", "x"}, cfg)
	if action != actionHarness || name != "pi" {
		t.Fatalf("harness precedence: want harness/pi, got %v/%q", action, name)
	}
	if len(args) != 1 || args[0] != "x" {
		t.Fatalf("harness precedence: want [x], got %v", args)
	}
}

func TestClassifyCmd_UnknownNoDefault(t *testing.T) {
	action, name, _ := classifyCmd([]string{"bogusprompt"}, cfgNoDefault())
	if action != actionUnknown || name != "bogusprompt" {
		t.Fatalf("unknown/no-default: want unknown/bogusprompt, got %v/%q", action, name)
	}
}

func TestClassifyCmd_Extension(t *testing.T) {
	dir := t.TempDir()
	writeExtension(t, dir, "ax-myext")
	t.Setenv("PATH", dir)

	action, name, _ := classifyCmd([]string{"myext", "arg"}, cfgNoDefault())
	if action != actionExtension || name != "myext" {
		t.Fatalf("extension: want extension/myext, got %v/%q", action, name)
	}
}

func TestClassifyCmd_ExtensionTakesPrecedenceOverDefault(t *testing.T) {
	// An ax-<cmd> binary on PATH should win over default_harness.
	dir := t.TempDir()
	writeExtension(t, dir, "ax-extfoo")
	t.Setenv("PATH", dir)

	cfg := cfgWithDefault("claude")
	action, name, _ := classifyCmd([]string{"extfoo"}, cfg)
	if action != actionExtension || name != "extfoo" {
		t.Fatalf("ext over default: want extension/extfoo, got %v/%q", action, name)
	}
}

func TestClassifyCmd_DashedVersion(t *testing.T) {
	action, name, _ := classifyCmd([]string{"--version"}, cfgNoDefault())
	if action != actionVerb || name != "--version" {
		t.Fatalf("--version: want verb/--version, got %v/%q", action, name)
	}
}

func TestClassifyCmd_DashedHelp(t *testing.T) {
	action, name, _ := classifyCmd([]string{"--help"}, cfgNoDefault())
	if action != actionVerb || name != "--help" {
		t.Fatalf("--help: want verb/--help, got %v/%q", action, name)
	}
}

func TestClassifyCmd_ShortHelp(t *testing.T) {
	action, name, _ := classifyCmd([]string{"-h"}, cfgNoDefault())
	if action != actionVerb || name != "-h" {
		t.Fatalf("-h: want verb/-h, got %v/%q", action, name)
	}
}

func TestClassifyCmd_UnrecognizedDashedArgGoesToPicker(t *testing.T) {
	action, _, _ := classifyCmd([]string{"--nonsense"}, cfgNoDefault())
	if action != actionPick {
		t.Fatalf("--nonsense: want pick, got %v", action)
	}
}

func TestIsKnownVerb(t *testing.T) {
	for _, v := range []string{"list", "kill", "read", "wait", "send", "runs", "metrics", "hook", "version", "help"} {
		if !isKnownVerb(v) {
			t.Errorf("isKnownVerb(%q) = false, want true", v)
		}
	}
	if isKnownVerb("bogus") {
		t.Error("isKnownVerb(bogus) = true, want false")
	}
	if isKnownVerb("claude") {
		t.Error("isKnownVerb(claude) = true, want false (it's a harness, not a verb)")
	}
}
