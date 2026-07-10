package notify

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// waitFor polls for a file to appear (the detached notify command writes it),
// returning its contents or failing after the deadline. The deadline is
// generous for Windows, where the detached command rides two PowerShell
// startups (Start-Process inside the backgrounding shell).
func waitFor(t *testing.T, path string) string {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("notify command never wrote %s", path)
	return ""
}

// writeTmpl builds a notify command template that writes the placeholders
// joined by "|" to path, in the platform shell Fire's command runs under:
// printf under POSIX sh, a Set-Content pipeline under PowerShell (Fire fills
// each placeholder as one quoted shell literal on either platform).
func writeTmpl(path string, placeholders ...string) string {
	if runtime.GOOS == "windows" {
		return "(" + strings.Join(placeholders, " + '|' + ") + ") | Set-Content -NoNewline -Path '" + path + "'"
	}
	return "printf '" + strings.TrimSuffix(strings.Repeat("%s|", len(placeholders)), "|") + "' " +
		strings.Join(placeholders, " ") + " > " + path
}

// writeLiteral builds a template that writes the literal text to path.
func writeLiteral(path, text string) string {
	if runtime.GOOS == "windows" {
		return "Set-Content -NoNewline -Path '" + path + "' -Value '" + text + "'"
	}
	return "printf '" + text + "' > " + path
}

// touchTmpl builds a template that creates path.
func touchTmpl(path string) string {
	if runtime.GOOS == "windows" {
		return "New-Item -ItemType File -Path '" + path + "'"
	}
	return "touch " + path
}

// A custom command in the string-shorthand form runs with template values filled.
func TestFireRunsCommand(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	Fire(Config{Attention: writeTmpl(out, "{state}", "{name}", "{run}")}, Event{
		State: NeedsYou, Name: "worker", Group: "blog",
	})
	if got := waitFor(t, out); got != "needs-you|worker|blog" {
		t.Fatalf("filled command wrong: %q", got)
	}
}

// {group} is a deprecated alias for {run}; both carry the same value.
func TestFireGroupIsDeprecatedAliasForRun(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	Fire(Config{Attention: writeTmpl(out, "{run}", "{group}")}, Event{
		State: NeedsYou, Group: "blog",
	})
	if got := waitFor(t, out); got != "blog|blog" {
		t.Fatalf("{run} and {group} did not carry the same value: %q", got)
	}
}

// The summary is agent-authored text; a shell metacharacter payload in it must
// be passed as data, never executed.
func TestFireDoesNotInjectSummary(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	pwned := filepath.Join(dir, "pwned")
	Fire(Config{Attention: writeTmpl(out, "{summary}")}, Event{
		State:   NeedsYou,
		Summary: "$(touch " + pwned + ")`touch " + pwned + "`",
	})
	got := waitFor(t, out)
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("summary payload executed: the pwned file was created")
	}
	if got != "$(touch "+pwned+")`touch "+pwned+"`" {
		t.Fatalf("summary not passed literally: %q", got)
	}
}

// An empty notify config is a no-op (does not panic or spawn anything).
func TestFireEmptyIsNoop(t *testing.T) {
	Fire(Config{}, Event{State: NeedsYou})
	Fire(Config{}, Event{State: DoneReview})
}

// The [notify] table form fires for a named event when its key is present.
func TestFireRunSuccessFromTable(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	cfg := Config{Events: map[string]string{
		RunSuccess: writeTmpl(out, "{event}", "{name}"),
	}}
	Fire(cfg, Event{State: RunSuccess, Name: "root"})
	if got := waitFor(t, out); got != "run-success|root" {
		t.Fatalf("table form wrong: %q", got)
	}
}

// The string shorthand (notify = "bell" / custom) must NOT fire for run-success;
// it is sugar for attention states only, so adding run-success to a table form
// is the only way to hook it.
func TestFireStringFormDoesNotFireRunSuccess(t *testing.T) {
	dir := t.TempDir()
	pwned := filepath.Join(dir, "pwned")
	Fire(Config{Attention: touchTmpl(pwned)}, Event{State: RunSuccess})
	// Give the (non-)command time to run if the guard is missing; the Windows
	// detached path rides two PowerShell startups, so it gets a longer window.
	wait := 60 * time.Millisecond
	if runtime.GOOS == "windows" {
		wait = 2 * time.Second
	}
	time.Sleep(wait)
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("string-form shorthand fired for run-success; it should only fire for attention states")
	}
}

// The string-form shorthand remains valid for attention states (back-compat).
func TestFireStringFormBackCompat(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	Fire(Config{Attention: writeLiteral(out, "ok")}, Event{State: NeedsYou})
	if got := waitFor(t, out); got != "ok" {
		t.Fatalf("string-form back-compat failed: %q", got)
	}
}

// {event} and {state} carry the same value so existing templates keep working.
func TestFireEventAndStatePlaceholdersMatch(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	Fire(Config{Events: map[string]string{
		DoneReview: writeTmpl(out, "{state}", "{event}"),
	}}, Event{State: DoneReview})
	if got := waitFor(t, out); got != "done-review|done-review" {
		t.Fatalf("{state} and {event} differ: %q", got)
	}
}
