package hold

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type attachResult struct {
	code int
	err  error
}

func TestAttachStdinEOFDetachesAndAllowsReattach(t *testing.T) {
	dir := tempHoldDir(t)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("AX_CONFIG", filepath.Join(dir, "config.toml"))

	origStdin := os.Stdin
	t.Cleanup(func() { os.Stdin = origStdin })

	id := fmt.Sprintf("stdin-eof-%d-%d", os.Getpid(), time.Now().UnixNano())
	rec := &inputRecorder{}
	srv, err := Serve(id, ServeOpts{Input: rec, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)

	firstStdin, firstInput, firstDone := startAttachOnPipe(t, id)
	waitForEOFTest(t, "first attach to reach the holder", func() bool { return len(srv.clientList()) == 1 })
	firstInput.Close()
	first := waitAttachEOFTest(t, "stdin EOF detach", firstDone, func() {
		firstInput.Close()
		firstStdin.Close()
		srv.Close()
	})
	firstStdin.Close()
	if first.err != nil || first.code != 0 {
		t.Fatalf("stdin EOF attach result = (%d, %v), want (0, nil)", first.code, first.err)
	}
	waitForEOFTest(t, "holder to drop the EOF-detached client", func() bool { return len(srv.clientList()) == 0 })
	if !Probe(id) {
		t.Fatal("holder gone after stdin EOF; it must keep running")
	}

	secondStdin, secondInput, secondDone := startAttachOnPipe(t, id)
	waitForEOFTest(t, "second attach to reach the holder", func() bool { return len(srv.clientList()) == 1 })
	if _, err := secondInput.Write([]byte("r")); err != nil {
		t.Fatalf("write second attach input: %v", err)
	}
	waitForEOFTest(t, "second attach input to reach the harness", func() bool { return rec.String() == "r" })
	if _, err := secondInput.Write([]byte{DefaultDetachPrefix, DefaultDetachLetter}); err != nil {
		t.Fatalf("write second attach detach chord: %v", err)
	}
	second := waitAttachEOFTest(t, "second attach detach", secondDone, func() {
		secondInput.Close()
		secondStdin.Close()
		srv.Close()
	})
	secondInput.Close()
	secondStdin.Close()
	if second.err != nil || second.code != 0 {
		t.Fatalf("second attach result = (%d, %v), want (0, nil)", second.code, second.err)
	}
	waitForEOFTest(t, "holder to drop the second client", func() bool { return len(srv.clientList()) == 0 })
	if !Probe(id) {
		t.Fatal("holder gone after reattach detach; it must keep running")
	}
}

func tempHoldDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp("/tmp", "axhold")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func startAttachOnPipe(t *testing.T, id string) (*os.File, *os.File, <-chan attachResult) {
	t.Helper()
	stdin, input, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	os.Stdin = stdin
	done := make(chan attachResult, 1)
	go func() {
		code, err := Attach(id, nil, nil)
		done <- attachResult{code: code, err: err}
	}()
	return stdin, input, done
}

func waitAttachEOFTest(t *testing.T, what string, done <-chan attachResult, cleanup func()) attachResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(5 * time.Second):
		cleanup()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	return attachResult{}
}

func waitForEOFTest(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
