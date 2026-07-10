//go:build windows

package app

import "os"

func waitCleanupSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func waitSignalExitCode(os.Signal) int {
	return 130
}
