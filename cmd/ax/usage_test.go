package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestUsageDocumentsAcceptedFlags(t *testing.T) {
	out := captureUsage(t)

	for _, flag := range []string{
		"--task-file", "--behavior", "--behavior-text", "--model", "--effort",
		"--run", "--group", "--name", "--parent", "--label", "--host", "--dir", "--accept",
		"--max-cost", "--max-tokens", "--max-workers", "--max-depth", "--timeout",
		"--wait", "--unattended", "--interactive", "--headless", "--json", "--api",
		"--clean-env", "--env", "--auth", "--close-on-done", "--keep-live", "--keep-live-for",
		"--write", "--no-write", "--no-subagents", "--fence", "--self-propel",
		"--propel-prompt", "--propel-until", "--done-check", "--max-idle-turns",
		"--propel-max-idle", "--propel-backoff", "--propel-watch",
		"--with-args", "--federated", "--hosts", "--args", "--force", "--older-than",
		"--stdin", "--rm-label", "--default", "--prom", "--ax", "--yes",
	} {
		if !strings.Contains(out, flag) {
			t.Fatalf("usage does not document %s\n%s", flag, out)
		}
	}
	if !strings.Contains(out, "--api is the deprecated alias for --auth api") {
		t.Fatal("usage must explain the deprecated --api alias")
	}
	if !strings.Contains(out, "--done-check for --propel-until") ||
		!strings.Contains(out, "--propel-max-idle for") {
		t.Fatal("usage must explain deprecated self-propel aliases")
	}
}

func captureUsage(t *testing.T) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(r)
		done <- result{data, err}
	}()
	usage()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res := <-done
	r.Close()
	if res.err != nil {
		t.Fatal(res.err)
	}
	return string(res.data)
}
