//go:build unix

package live

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// hardKill: terminate delivers a catchable SIGTERM, so the wrapper runs its
// own teardown (including removing the liveness record); Kill leaves it alone.
const hardKill = false

// terminate stops a running session by signalling its ax-run wrapper pid, which
// forwards SIGTERM to the harness and clears the record on exit.
func terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err == nil {
		stat := strings.TrimSpace(string(out))
		return stat != "" && !strings.HasPrefix(stat, "Z")
	}
	err = syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// processStartToken returns an immutable process-start identity for pid. Darwin
// uses kern.proc.pid, Linux uses /proc starttime, and ps is a last-resort Unix
// fallback for platforms without a kernel-specific implementation here.
func processStartToken(pid int) string {
	if pid <= 0 {
		return ""
	}
	if token := kernelProcessStartToken(pid); token != "" {
		return token
	}
	if token := procStatStartToken(pid); token != "" {
		return "procstat:" + token
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	start := strings.TrimSpace(string(out))
	if start == "" {
		return ""
	}
	return "pslstart:" + start
}

func procStatStartToken(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	stat := string(data)
	end := strings.LastIndex(stat, ") ")
	if end < 0 {
		return ""
	}
	fields := strings.Fields(stat[end+2:])
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}

// pidIsAxRun reports whether pid is still an ax run wrapper (or a dtach holder
// in front of one), guarding Kill against pid reuse after an uncleaned death.
func pidIsAxRun(pid int) bool {
	return axRunSessionArg(pidCommand(pid)) != ""
}

func pidCommand(pid int) string {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "" // no such process
	}
	return strings.TrimSpace(string(out))
}

func axRunSessionArg(cmd string) string {
	fields := strings.Fields(cmd)
	for i := 0; i+1 < len(fields); i++ {
		if !looksLikeAx(fields[i]) || fields[i+1] != "run" {
			continue
		}
		for j := i + 2; j < len(fields); {
			switch fields[j] {
			case "--hold":
				j++
			case "--adopt":
				j += 2
			default:
				return fields[j]
			}
		}
	}
	return ""
}

func looksLikeAx(s string) bool {
	base := filepath.Base(strings.Trim(s, `"'`))
	return base == "ax" || base == "ax.exe" || strings.HasPrefix(base, "ax.") || strings.HasPrefix(base, "ax-")
}
