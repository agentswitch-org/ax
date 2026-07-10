//go:build windows

package hold

import (
	"fmt"
	"net"
	"time"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// nativeSupported: the native holder runs on Windows over a per-session named
// pipe (go-winio) fronting a ConPTY (internal/hold/conpty). Same server,
// protocol, and ring as unix; only this transport seam differs.
const nativeSupported = true

// probeTimeout bounds the detach-safety dial probe, so a wedged holder reads
// as unheld instead of stalling the picker action that asked.
const probeTimeout = 250 * time.Millisecond

// listen takes session id's named pipe for a new holder. A live pipe (another
// holder answering) is an error, mirroring the unix socket path; there is no
// stale-file case, because a pipe name vanishes with its last handle.
func listen(id string) (net.Listener, error) {
	name := pipeName(id)
	if c, err := dialTimeout(id, probeTimeout); err == nil {
		c.Close()
		return nil, fmt.Errorf("session %s is already held (pipe %s answers)", id, name)
	}
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{
		// Session control stays private: SYSTEM and the owning user, nobody
		// else (the unix side's 0600 socket in a 0700 dir). Empty on a token
		// failure, which falls back to winio's default descriptor.
		SecurityDescriptor: ownerSDDL(),
		// Zero-size buffers make npfs rendezvous every WriteFile with a
		// pending ReadFile on the far end; a holder writing OUTPUT with no
		// posted reader would stall until the write deadline dropped the
		// client. Real buffers give the unix socket's decoupling: small
		// writes complete into the buffer immediately.
		InputBufferSize:  64 * 1024,
		OutputBufferSize: 64 * 1024,
	})
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", name, err)
	}
	return ln, nil
}

// dial connects to session id's holder.
func dial(id string) (net.Conn, error) { return winio.DialPipe(pipeName(id), nil) }

func dialTimeout(id string, d time.Duration) (net.Conn, error) {
	return winio.DialPipe(pipeName(id), &d)
}

// Probe reports whether a holder is answering on session id's pipe: the
// ground-truth held check (on Windows a stat cannot even see a pipe; only a
// dial can).
func Probe(id string) bool {
	c, err := dialTimeout(id, probeTimeout)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// ownerSDDL is the pipe's owner-only DACL: full access for SYSTEM and the
// current user, protected against inheritance, denying every other account on
// the machine. "" (winio's default descriptor) on a token read failure.
func ownerSDDL() string {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return ""
	}
	return "D:P(A;;GA;;;SY)(A;;GA;;;" + u.User.Sid.String() + ")"
}
