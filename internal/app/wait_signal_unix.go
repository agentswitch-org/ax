//go:build unix

package app

import (
	"os"
	"syscall"
)

func waitCleanupSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

func waitSignalExitCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 130
}
