//go:build unix && !darwin

package live

func kernelProcessStartToken(pid int) string { return "" }
