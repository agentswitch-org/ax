// Package conpty owns the harness's Windows pseudoconsole: it creates a
// ConPTY (CreatePseudoConsole), spawns the harness attached to it via
// STARTUPINFOEX + PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, and exposes the
// console's read/write pipes and a resize call. It is the holder's Windows
// analog of creack/pty's Start on unix (creack/pty ships only a
// non-functional Windows stub). The package knows nothing of sessions or the
// hold protocol: it runs one program under one pseudoconsole.
//
// Lifecycle rules that differ from a unix pty, per the ConPTY docs:
//
//   - Read returns EOF only after Close, not when the program exits: conhost
//     keeps the output pipe open. Detect exit with Wait, then Close.
//   - Close terminates the attached process tree (ClosePseudoConsole sends
//     CTRL_CLOSE_EVENT); a harness cannot survive its holder on Windows.
//   - The caller must keep draining Read while Close runs (the holder's read
//     loop does naturally): pre-24H2, ClosePseudoConsole blocks until the
//     output pipe is drained or closed.
package conpty

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// Default size seeded at birth, matching the holder's detached-run seed (a
// 0x0 console makes a harness sit waiting instead of running).
const (
	defaultRows = 40
	defaultCols = 120
)

// clampDim narrows a protocol dimension (uint16) to a COORD field (int16),
// clamping instead of overflowing negative on absurd values.
func clampDim(v uint16) int16 {
	if v > 32767 {
		return 32767
	}
	return int16(v)
}

// envBlock builds the CREATE_UNICODE_ENVIRONMENT block for CreateProcess:
// each "key=value" UTF-16 encoded and NUL-terminated, the block double-NUL
// terminated. nil env means nil block (the child inherits this process's
// environment).
func envBlock(env []string) ([]uint16, error) {
	if env == nil {
		return nil, nil
	}
	if len(env) == 0 {
		return []uint16{0, 0}, nil
	}
	var b []uint16
	for _, kv := range env {
		if !strings.Contains(kv, "=") {
			return nil, fmt.Errorf("conpty: malformed environment entry %q", kv)
		}
		if strings.ContainsRune(kv, 0) {
			return nil, fmt.Errorf("conpty: environment entry %q contains NUL", kv)
		}
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	return append(b, 0), nil
}
