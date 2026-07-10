//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package hold

import "golang.org/x/sys/unix"

const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)
