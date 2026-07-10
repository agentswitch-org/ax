// Package live tracks which sessions are running and whether they are working.
// Each session ax launched has a file under ~/.local/state/ax/live/<id>:
//   - the file's mtime is the heartbeat (bumped on a timer): fresh = alive, a
//     stale mtime means the session died without a clean exit (a crash).
//   - the file's content is "<last-output-unix>\t<pid>\t<start-token>\t<command>":
//     last-output is bumped whenever the harness writes to the terminal (recent =
//     working), pid is the ax-run wrapper's pid, and start-token is that process's
//     immutable start identity. Kill signals the pid only when both still match.
//
// One file per session avoids any concurrent-write contention.
package live

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
)

const (
	Interval = 15 * time.Second // heartbeat (liveness) cadence
	Fresh    = 50 * time.Second // a heartbeat within this is alive
	Active   = 5 * time.Second  // output within this means working, else idle
)

func dir() string { return axdir.State("live") }

func readDir() string { return axdir.StatePath("live") }

var selfStart struct {
	once  sync.Once
	token string
}

func write(id string, lastOutput int64, cmd string) {
	selfStart.once.Do(func() { selfStart.token = processStartToken(os.Getpid()) })
	rec := strings.Join([]string{
		strconv.FormatInt(lastOutput, 10),
		strconv.Itoa(os.Getpid()),
		selfStart.token,
		cmd,
	}, "\t")
	axdir.WriteFileAtomic(filepath.Join(dir(), id), []byte(rec), 0o600)
}

// Start records a launched session (marks it active now).
func Start(id, cmd string) { write(id, time.Now().Unix(), cmd) }

// Output marks the session as having just produced terminal output (working).
func Output(id, cmd string) { write(id, time.Now().Unix(), cmd) }

// Touch bumps only the heartbeat mtime (liveness), preserving last-output, so an
// alive-but-quiet session reads as idle rather than working.
func Touch(id string) {
	now := time.Now()
	os.Chtimes(filepath.Join(dir(), id), now, now)
}

// Remove clears a session's record on clean exit.
func Remove(id string) { os.Remove(filepath.Join(readDir(), id)) }

// Kill stops a running session by signalling its ax-run wrapper, which forwards
// SIGTERM to the harness and clears this record on exit. A stale record (a crash
// with no live process) is just removed. Safe to call on an unknown id. This is
// the explicit teardown: closing a viewer window only detaches the holder.
func Kill(id string) error {
	e, ok := Snapshot()[id]
	if !ok {
		return nil
	}
	if e.Age > Fresh { // stale: a crash record with no live process, just clear it
		Remove(id)
		return nil
	}
	if e.PID == 0 { // fresh record from an older ax: cannot signal, must not delete
		return fmt.Errorf("no pid recorded (started by an older ax); close its window or stop it manually")
	}
	if !sameProcess(e) {
		// The recorded wrapper died without cleanup (a hard kill, a crash) and
		// the OS may have recycled its pid onto another process, including a
		// different ax-run wrapper. Refusing beats signalling an innocent
		// bystander. Clear the dead record.
		Remove(id)
		return nil
	}
	if err := terminate(e.PID); err != nil {
		return err
	}
	if hardKill { // the wrapper died without its own teardown; clear the record for it
		Remove(id)
	}
	return nil
}

// Entry is a recorded session: its command, time since the last heartbeat, the
// unix time of its last terminal output, the ax-run wrapper's pid, and the
// wrapper's immutable start token.
type Entry struct {
	Cmd        string
	Age        time.Duration
	LastOutput int64
	PID        int
	StartToken string
}

// Snapshot returns every recorded session keyed by id.
func Snapshot() map[string]Entry {
	out := map[string]Entry{}
	d := readDir()
	es, err := os.ReadDir(d)
	if err != nil {
		return out
	}
	now := time.Now()
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".") {
			continue // an atomic-write temp file (.ax-*), not a session record
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(d, e.Name()))
		lo, pid, token, cmd := parse(string(data))
		out[e.Name()] = Entry{Cmd: cmd, Age: now.Sub(info.ModTime()), LastOutput: lo, PID: pid, StartToken: token}
	}
	return out
}

// LiveIDs is the set of session ids whose heartbeat is fresh (running now).
func LiveIDs() map[string]bool {
	out := map[string]bool{}
	for id, e := range Snapshot() {
		if Running(e) {
			out[id] = true
		}
	}
	return out
}

// Running reports whether a heartbeat is fresh and, when it carries a wrapper
// pid, that pid still names the same ax run wrapper process. Checking a stable
// process-start token instead of the command shape alone keeps a stale heartbeat
// from reading live after the OS reuses the recorded pid for another ax run.
// Legacy pre-pid records cannot be verified, so they retain the old age-only
// behavior while they are fresh.
func Running(e Entry) bool {
	if e.Age > Fresh {
		return false
	}
	if e.PID == 0 {
		return true
	}
	if e.StartToken == "" {
		return pidIsAxRun(e.PID)
	}
	return sameProcess(e)
}

func sameProcess(e Entry) bool {
	if e.PID <= 0 || e.StartToken == "" || processStartToken(e.PID) != e.StartToken {
		return false
	}
	return pidIsAxRun(e.PID)
}

func parse(s string) (lastOutput int64, pid int, startToken, cmd string) {
	parts := strings.SplitN(strings.TrimRight(s, "\n"), "\t", 4)
	lastOutput, _ = strconv.ParseInt(parts[0], 10, 64)
	switch len(parts) {
	case 2: // legacy "<lastOutput>\t<cmd>" (pre-pid records)
		cmd = parts[1]
	case 3: // legacy "<lastOutput>\t<pid>\t<cmd>" (pre-start-token records)
		pid, _ = strconv.Atoi(parts[1])
		cmd = parts[2]
	case 4:
		pid, _ = strconv.Atoi(parts[1])
		startToken = parts[2]
		cmd = parts[3]
	}
	return
}
