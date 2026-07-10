//go:build windows

package app

func platformScanAdoptedWrappers() []adoptedWrapper { return nil }

func platformKillAdoptedWrapper(adoptedWrapper) error { return nil }
