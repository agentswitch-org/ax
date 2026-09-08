package propel

import (
	"context"
	"strings"
	"testing"
)

func TestCheckRetainsFailureEvidenceAndExitStatus(t *testing.T) {
	got := RunCheck("echo specific-failure; exit 7", t.TempDir())
	if got.Passed || !strings.Contains(got.Output, "specific-failure") {
		t.Fatalf("check: %+v", got)
	}
	if got := RunCheck("exit 0", t.TempDir()); !got.Passed {
		t.Fatalf("pass: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := runCheck(ctx, "exit 0", t.TempDir()); got.Passed || !strings.Contains(got.Output, "interrupted") {
		t.Fatalf("cancel: %+v", got)
	}
}

func TestCheckOutputIsBoundedAndKeepsLastError(t *testing.T) {
	var out checkTail
	out.Write([]byte(strings.Repeat("x", 10000)))
	out.Write([]byte("the last failure"))
	if len(out) > 2048 || !strings.HasSuffix(string(out), "the last failure") {
		t.Fatal("check output must retain a bounded tail")
	}
}
