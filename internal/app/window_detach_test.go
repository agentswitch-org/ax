package app

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/agentswitch-org/ax/internal/hold"
	"github.com/agentswitch-org/ax/internal/session"
)

// windowDetachSafe is the guard that keeps a bulk detach from ever killing a
// session: closing a window only detaches a held process, so an unheld local
// window (no holder answering on the session socket) must be skipped rather
// than closed. A remote session is always safe (it lives on its owner; the
// local window is just a view).
func TestWindowDetachSafe(t *testing.T) {
	// Point the holder socket dir at a SHORT temp location (a unix socket path is
	// capped near 104 bytes on macOS, which t.TempDir()'s long path blows) and keep
	// the process backend off, so held-ness is decided purely by the endpoint probe.
	tmpRoot := "/tmp"
	if runtime.GOOS == "windows" {
		tmpRoot = "" // %TEMP%; the Windows endpoint is a named pipe, no path cap
	}
	base, err := os.MkdirTemp(tmpRoot, "axs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	t.Setenv("XDG_RUNTIME_DIR", base)
	if runtime.GOOS == "windows" {
		// The Windows default mux is the process backend, under which no local
		// window is ever detach-safe; pin tmux so the probe seam under test here
		// is reachable, matching the unix default.
		cfg := filepath.Join(base, "cfg.toml")
		if err := os.WriteFile(cfg, []byte("mux = \"tmux\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AX_CONFIG", cfg)
	} else {
		t.Setenv("AX_CONFIG", filepath.Join(base, "absent.toml")) // no config => mux tmux, hold native
	}

	// Remote session: safe regardless of any local socket.
	if !windowDetachSafe(session.Session{ID: "r1", Host: "box"}) {
		t.Error("remote session reported not detach-safe; closing its local window only detaches the view")
	}

	// Local session with NO holder socket: closing its window would kill the
	// process, so it must be reported unsafe (skip it).
	if windowDetachSafe(session.Session{ID: "unheld"}) {
		t.Error("local session with no holder reported detach-safe; closing it would KILL the process")
	}

	// Local session with a STALE socket file (its holder died without cleanup):
	// nothing answers the probe, so it must read as unheld. A stat-based check
	// would get exactly this wrong. Unix-only: a Windows pipe name vanishes with
	// its last handle, so there is no stale-endpoint case to defend against.
	if runtime.GOOS != "windows" {
		stale := hold.Sock("stale")
		if f, err := os.Create(stale); err == nil { // a dead plain file where the socket was
			f.Close()
		}
		if windowDetachSafe(session.Session{ID: "stale"}) {
			t.Error("local session with a dead socket file reported detach-safe; nothing is holding it")
		}
	}

	// Local session WITH a live holder answering on its endpoint: held, so closing
	// only detaches.
	if runtime.GOOS == "windows" {
		// Only a pipe listener can create the named-pipe endpoint; a real (idle)
		// holder server stands in.
		srv, err := hold.Serve("held", hold.ServeOpts{})
		if err != nil {
			t.Fatalf("create holder stand-in: %v", err)
		}
		defer srv.Close()
	} else {
		sock := hold.Sock("held") // creates the parent dir; we stand in for the holder
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatalf("create holder stand-in at %s: %v", sock, err)
		}
		defer ln.Close()
	}
	if !windowDetachSafe(session.Session{ID: "held"}) {
		t.Error("local session with a live holder reported unsafe; closing it would only detach")
	}
}
