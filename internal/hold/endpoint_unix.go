//go:build unix

package hold

import (
	"fmt"
	"net"
	"os"
	"time"
)

// nativeSupported: the native holder runs anywhere with unix sockets and ptys.
// Windows needs the ConPTY + named-pipe milestone.
const nativeSupported = true

// probeTimeout bounds the detach-safety dial probe, so a wedged holder reads
// as unheld instead of stalling the picker action that asked.
const probeTimeout = 250 * time.Millisecond

// listen takes session id's socket for a new holder. A live socket (another
// holder answering) is an error; a stale file (its holder died without
// cleanup) is unlinked and replaced, dtach -A style.
func listen(id string) (net.Listener, error) {
	sock := Sock(id)
	if c, err := net.DialTimeout("unix", sock, probeTimeout); err == nil {
		c.Close()
		return nil, fmt.Errorf("session %s is already held (socket %s answers)", id, sock)
	}
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	os.Chmod(sock, 0o600) // session control stays private (the dir already is)
	return ln, nil
}

// dial connects to session id's holder.
func dial(id string) (net.Conn, error) { return net.Dial("unix", Sock(id)) }

// Probe reports whether a holder is answering on session id's socket: the
// ground-truth held check (a stat cannot tell a live holder from a stale
// file). It works against both the native holder and a dtach master, which
// also accepts-and-forgets a connection that closes without a packet.
func Probe(id string) bool {
	c, err := net.DialTimeout("unix", Sock(id), probeTimeout)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
