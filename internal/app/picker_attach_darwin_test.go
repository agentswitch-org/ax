//go:build darwin

package app

import (
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

func processStartTokenForTest(t *testing.T, pid int) string {
	t.Helper()
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || kp.Proc.P_pid != int32(pid) {
		t.Fatalf("read start token for pid %d: %v", pid, err)
	}
	tv := kp.Proc.P_starttime
	if tv.Sec == 0 && tv.Usec == 0 {
		t.Fatalf("pid %d has empty process start time", pid)
	}
	return "darwinstart:" + strconv.FormatInt(tv.Sec, 10) + "." + strconv.FormatInt(int64(tv.Usec), 10)
}
