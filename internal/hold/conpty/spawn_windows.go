//go:build windows

package conpty

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procIsProcessInJob = kernel32.NewProc("IsProcessInJob")

// DetachedSysProcAttr is the creation attribute for spawning a long-lived
// holder that must survive whatever launched it: the setsid analog, shared by
// the mux process backend's Open and the app's detached holder spawn. It
// lives here beside the ConPTY plumbing because it is the same Windows
// console/process lore, win01-verified as a set:
//   - CREATE_NO_WINDOW: a fresh hidden console of its own, so no console
//     close or ctrl event from the launcher's world reaches it. (Not
//     DETACHED_PROCESS: a console-program chain like pwsh dies during startup
//     with no console at all. Not CREATE_NEW_PROCESS_GROUP: that silently
//     disables ctrl-c handling for every descendant, breaking Interrupt.)
//   - CREATE_BREAKAWAY_FROM_JOB, only when this process sits in a job that
//     permits breakaway: sshd wraps every session's processes in a
//     kill-on-close job, so without breakaway a detached holder dies with the
//     ssh session that launched it. The probe keeps the flag off when it
//     would make CreateProcess fail (no job, or breakaway denied).
func DetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | breakawayFlag(),
	}
}

// breakawayFlag reports CREATE_BREAKAWAY_FROM_JOB when this process is in a
// job whose limits permit breakaway, else 0. A job with SILENT_BREAKAWAY_OK
// releases children on its own, so only explicit BREAKAWAY_OK needs the flag.
func breakawayFlag() uint32 {
	var inJob int32
	if r, _, _ := procIsProcessInJob.Call(uintptr(windows.CurrentProcess()), 0, uintptr(unsafe.Pointer(&inJob))); r == 0 || inJob == 0 {
		return 0
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var ret uint32
	if err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &ret); err != nil {
		return 0
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK != 0 {
		return windows.CREATE_BREAKAWAY_FROM_JOB
	}
	return 0
}
