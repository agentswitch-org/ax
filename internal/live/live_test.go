package live

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStartSnapshotLiveIDsAndRemove(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	Start("s1", "ax run")
	snap := Snapshot()
	e, ok := snap["s1"]
	if !ok {
		t.Fatalf("Snapshot missing s1: %#v", snap)
	}
	if e.Cmd != "ax run" || e.PID != os.Getpid() || e.StartToken == "" || e.LastOutput == 0 || e.Age > Fresh {
		t.Fatalf("Snapshot entry = %#v, want fresh current process", e)
	}
	stale := time.Now().Add(-Fresh - time.Second)
	if err := os.Chtimes(filepath.Join(readDir(), "s1"), stale, stale); err != nil {
		t.Fatal(err)
	}
	if LiveIDs()["s1"] {
		t.Fatalf("LiveIDs included stale s1: %#v", LiveIDs())
	}

	Output("s1", "ax run updated")
	if got := Snapshot()["s1"]; got.Cmd != "ax run updated" || got.Age > Fresh {
		t.Fatalf("after Output entry = %#v", got)
	}
	Touch("s1")
	if got := Snapshot()["s1"]; got.Age > Fresh {
		t.Fatalf("after Touch entry = %#v, want fresh", got)
	}

	Remove("s1")
	if _, ok := Snapshot()["s1"]; ok {
		t.Fatalf("Snapshot still contains s1 after Remove: %#v", Snapshot())
	}
}

func TestLiveIDsKeepsFreshLegacyNoPIDRecords(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	now := time.Now().Unix()
	if err := os.WriteFile(filepath.Join(dir(), "legacy"), []byte(fmt.Sprintf("%d\tlegacy command", now)), 0o600); err != nil {
		t.Fatal(err)
	}
	if !LiveIDs()["legacy"] {
		t.Fatalf("LiveIDs = %#v, want fresh legacy record", LiveIDs())
	}
}

func TestParseCurrentAndLegacyRecords(t *testing.T) {
	lo, pid, token, cmd := parse("123\t456\tstart-token\tax run --flag")
	if lo != 123 || pid != 456 || token != "start-token" || cmd != "ax run --flag" {
		t.Fatalf("parse current = %d/%d/%q/%q", lo, pid, token, cmd)
	}

	lo, pid, token, cmd = parse("234\t567\tax run old")
	if lo != 234 || pid != 567 || token != "" || cmd != "ax run old" {
		t.Fatalf("parse legacy pid = %d/%d/%q/%q", lo, pid, token, cmd)
	}

	lo, pid, token, cmd = parse("789\tlegacy command")
	if lo != 789 || pid != 0 || token != "" || cmd != "legacy command" {
		t.Fatalf("parse legacy no-pid = %d/%d/%q/%q", lo, pid, token, cmd)
	}
}

func TestRunningRequiresFreshLivePIDWhenRecorded(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if !Running(Entry{Age: 0}) {
		t.Fatal("legacy no-pid fresh entries retain age-only live behavior")
	}
	if Running(Entry{Age: Fresh + time.Second, PID: os.Getpid()}) {
		t.Fatal("stale entry must not verify as running")
	}
	dead := deadPID()
	if Running(Entry{Age: 0, PID: dead}) {
		t.Fatalf("dead pid %d verified as running", dead)
	}

	now := time.Now().Unix()
	if err := os.WriteFile(filepath.Join(dir(), "dead"), []byte(fmt.Sprintf("%d\t%d\tax run", now, dead)), 0o600); err != nil {
		t.Fatal(err)
	}
	if LiveIDs()["dead"] {
		t.Fatal("LiveIDs included a fresh heartbeat whose recorded pid is gone")
	}
}

func deadPID() int {
	pid := 99999999
	for pidAlive(pid) {
		pid++
	}
	return pid
}
