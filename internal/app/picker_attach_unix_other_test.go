//go:build unix && !darwin

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func processStartTokenForTest(t *testing.T, pid int) string {
	t.Helper()
	if token := procStatStartTokenForTest(pid); token != "" {
		return "procstat:" + token
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("read ps start time for pid %d: %v", pid, err)
	}
	start := strings.TrimSpace(string(out))
	if start == "" {
		t.Fatalf("pid %d has empty process start time", pid)
	}
	return "pslstart:" + start
}

func procStatStartTokenForTest(pid int) string {
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
