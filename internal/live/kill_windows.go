//go:build windows

package live

import (
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

// hardKill: terminate cannot deliver a catchable signal on Windows, so the
// wrapper dies without running its own teardown and Kill removes the liveness
// record on its behalf. The harness itself still dies: the dying wrapper's
// ConPTY handles close, which terminates the attached process tree.
const hardKill = true

// terminate stops a running session by killing its ax-run wrapper. Windows has
// no signals, so it terminates the process directly rather than delivering
// SIGTERM.
func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func pidAlive(pid int) bool {
	return tasklistPID(pid) != ""
}

// processStartToken returns Windows' process creation time. The live record
// already carries the pid, and pid+creation-time is the immutable identity that
// keeps PID reuse from binding an old session to a new ax.exe process.
func processStartToken(pid int) string {
	if pid <= 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return ""
	}
	return "winfiletime:" + strconv.FormatInt(creation.Nanoseconds(), 10)
}

// pidIsAxRun reports whether pid is still an ax executable. The start-token
// check binds the exact process; this image-name check is only a secondary guard
// against a malformed heartbeat pointing at some non-ax process.
func pidIsAxRun(pid int) bool {
	return strings.HasPrefix(tasklistPID(pid), `"ax`)
}

func tasklistPID(pid int) string {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if s == "" || strings.HasPrefix(strings.ToUpper(s), "INFO:") {
		return ""
	}
	return s
}
