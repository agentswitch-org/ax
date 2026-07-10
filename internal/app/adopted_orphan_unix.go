//go:build unix

package app

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func platformScanAdoptedWrappers() []adoptedWrapper {
	out, err := exec.Command("ps", "-axo", "pid=,pgid=,stat=,command=").Output()
	if err != nil {
		return nil
	}
	var wrappers []adoptedWrapper
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || strings.HasPrefix(fields[2], "Z") {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		pgid, _ := strconv.Atoi(fields[1])
		cmd := strings.Join(fields[3:], " ")
		launchID, ok := adoptedLaunchIDFromCommand(cmd)
		if !ok {
			continue
		}
		wrappers = append(wrappers, adoptedWrapper{PID: pid, PGID: pgid, LaunchID: launchID, Command: cmd})
	}
	return wrappers
}

func adoptedLaunchIDFromCommand(cmd string) (string, bool) {
	fields := strings.Fields(cmd)
	for i := 0; i+1 < len(fields); i++ {
		if !looksLikeAxCommand(fields[i]) || fields[i+1] != "run" {
			continue
		}
		adopted := false
		for j := i + 2; j < len(fields); {
			switch fields[j] {
			case "--hold":
				j++
			case "--adopt":
				if j+1 >= len(fields) {
					return "", false
				}
				adopted = true
				j += 2
			default:
				if adopted {
					return strings.Trim(fields[j], `"'`), true
				}
				return "", false
			}
		}
	}
	return "", false
}

func looksLikeAxCommand(s string) bool {
	base := filepath.Base(strings.Trim(s, `"'`))
	return base == "ax" || base == "ax.exe" || strings.HasPrefix(base, "ax.") || strings.HasPrefix(base, "ax-")
}

func platformKillAdoptedWrapper(w adoptedWrapper) error {
	if w.PGID > 0 && w.PGID == w.PID {
		if err := syscall.Kill(-w.PGID, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	if err := syscall.Kill(w.PID, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
