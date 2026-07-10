// Package axlog writes ax's diagnostic log to a size-rotated file under the state
// dir. Errors that otherwise scroll past in a closing window (a harness that
// crashed on launch, a failed kill, an unreachable host) are recorded here and
// read back with `ax log`. It is best-effort and never fails a caller.
package axlog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
)

// maxSize is the point at which the log rotates: the current file is moved to
// "<log>.1" (overwriting the previous one) and a fresh file starts. So at most
// two files exist, a simple round-robin bounded to ~2x this.
const maxSize = 1 << 20 // 1 MiB

var mu sync.Mutex

// Path is the current log file. The previous one is Path()+".1".
func Path() string { return filepath.Join(axdir.State(), "log") }

// Printf appends a timestamped line to the log, rotating first if it is full.
func Printf(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	p := Path()
	if fi, err := os.Stat(p); err == nil && fi.Size() > maxSize {
		os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// Dump writes the log (previous file then current, oldest first) to w-ish; it
// returns the combined contents for `ax log`.
func Dump() []byte {
	var out []byte
	for _, p := range []string{Path() + ".1", Path()} {
		if data, err := os.ReadFile(p); err == nil {
			out = append(out, data...)
		}
	}
	return out
}
