// Package hold is the persistence layer for a session: a holder keeps the
// harness (under its ax-run heartbeat wrapper) alive across terminal
// disconnects, so closing a viewer window detaches instead of killing. The
// native backend folds the holder into `ax run` itself: the wrapper listens on
// a per-session unix socket, keeps a bounded ring of recent pty output for
// reattach repaint, and serves a framed attach protocol (proto.go) to `ax
// attach` clients. The dtach backend is the pre-native behavior kept as a
// field fallback (hold_backend = "dtach"); "none" disables holding entirely
// (closing the window kills the session). Holding is not multiplexing: the
// access point's tmux is the multiplexer, this only keeps one process alive.
package hold

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/agentswitch-org/ax/internal/axdir"
	"github.com/agentswitch-org/ax/internal/config"
)

// The attach client's detach keystroke is a screen-style two-key chord:
// prefix then command letter, Ctrl-A then d by default. A single key cannot
// work here: any control byte that survives a nested pty chain (podman/docker
// exec, a terminal whose raw mode did not take) is a letter-class byte, and
// those are exactly the bytes the harnesses bind (Ctrl-G opens Claude Code's
// editor, Ctrl-A is readline start-of-line), while the bytes the harnesses do
// not use are signal or flow-control keys the tty driver eats (Ctrl-\ becomes
// SIGQUIT, Ctrl-Q is swallowed as XON; both measured through `podman exec
// -it`). A prefix chord fixes both sides: the prefix byte survives every tty
// layer as a plain byte, only the prefix is intercepted, and pressing it
// twice sends one literal prefix through, so no harness key is permanently
// stolen. The detach_prefix config setting rebinds the prefix to any
// ctrl-<letter>; detach_key rebinds the command letter.
const (
	DefaultDetachPrefix byte = 0x01 // Ctrl-A, the screen prefix
	DefaultDetachLetter byte = 'd'  // d for detach, as in screen
	DefaultMenuLetter   byte = 'a'  // a for "ax picker": detach then reopen the TUI
)

// detachFallback is Ctrl-backslash (0x1c, the historic dtach default): kept as
// a harmless always-on alias so old habits still detach. It is also the byte
// a terminal whose raw mode did not take turns into SIGQUIT, which the attach
// client's signal handler catches the same way.
const detachFallback byte = 0x1c

// ParseDetachKey maps a detach_prefix config value to its control byte: a
// "ctrl-<letter>" name or the caret form "^<letter>", case-insensitive.
func ParseDetachKey(s string) (byte, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	var letter byte
	switch {
	case strings.HasPrefix(s, "ctrl-") && len(s) == len("ctrl-")+1:
		letter = s[len("ctrl-")]
	case strings.HasPrefix(s, "^") && len(s) == 2:
		letter = s[1]
	default:
		return 0, false
	}
	if letter < 'a' || letter > 'z' {
		return 0, false
	}
	return letter - 'a' + 1, true
}

// ParseDetachLetter maps a detach_key config value to the chord's command
// letter: a bare letter ("d"), case-insensitive. A "ctrl-<letter>" or caret
// form (the pre-chord detach_key syntax) is accepted as its letter, so an old
// detach_key = "ctrl-g" keeps working as prefix-then-g.
func ParseDetachLetter(s string) (byte, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(s, "ctrl-") && len(s) == len("ctrl-")+1:
		s = s[len("ctrl-"):]
	case strings.HasPrefix(s, "^") && len(s) == 2:
		s = s[1:]
	}
	if len(s) != 1 || s[0] < 'a' || s[0] > 'z' {
		return 0, false
	}
	return s[0], true
}

// DetachPrefixByte resolves a detach_prefix setting to the chord's prefix
// byte, falling back to Ctrl-A on empty or unparseable values.
func DetachPrefixByte(setting string) byte {
	if b, ok := ParseDetachKey(setting); ok {
		return b
	}
	return DefaultDetachPrefix
}

// DetachLetterByte resolves a detach_key setting to the chord's command
// letter, falling back to d on empty or unparseable values.
func DetachLetterByte(setting string) byte {
	if b, ok := ParseDetachLetter(setting); ok {
		return b
	}
	return DefaultDetachLetter
}

// MenuLetterByte resolves a menu_key setting to the menu chord's command
// letter, falling back to a on empty or unparseable values. The menu chord
// shares the detach prefix (ParseDetachLetter accepts the same bare-letter and
// legacy ctrl-/caret forms as detach_key).
func MenuLetterByte(setting string) byte {
	if b, ok := ParseDetachLetter(setting); ok {
		return b
	}
	return DefaultMenuLetter
}

// DetachLabel is the human name of the configured detach chord ("Ctrl-A then
// d"), for the attach hint, the picker hint row, and the help screen.
func DetachLabel(prefixSetting, keySetting string) string {
	p := DetachPrefixByte(prefixSetting)
	return "Ctrl-" + string(rune('A'+p-1)) + " then " + string(rune(DetachLetterByte(keySetting)))
}

// MenuLabel is the human name of the configured menu chord ("Ctrl-A then a"),
// which detaches and reopens the ax picker. It shares the detach prefix.
func MenuLabel(prefixSetting, keySetting string) string {
	p := DetachPrefixByte(prefixSetting)
	return "Ctrl-" + string(rune('A'+p-1)) + " then " + string(rune(MenuLetterByte(keySetting)))
}

// Backends selectable via the hold_backend config setting.
const (
	BackendNative = "native" // ax's own holder inside `ax run` (default)
	BackendDtach  = "dtach"  // the external dtach binary (pre-native fallback)
	BackendNone   = "none"   // no holder: closing the window kills the session
)

// Backend returns the configured holder backend. Unset and unknown values mean
// native, which exists on every platform (unix sockets + ptys; named pipes +
// ConPTY on Windows). Were a platform ever built without it, native would
// degrade to dtach, which degrades further through Available() when the
// binary is absent, reproducing the old unheld behavior.
func Backend() string {
	cfg, _ := config.Load()
	b := cfg.HoldBackend
	switch b {
	case BackendDtach, BackendNone:
		return b
	}
	if !nativeSupported {
		return BackendDtach
	}
	return BackendNative
}

// Path returns the dtach binary path and whether it is installed.
func Path() (string, bool) {
	p, err := exec.LookPath("dtach")
	if err != nil {
		return "", false
	}
	return p, true
}

// Available reports whether sessions can be held: always with the native
// backend, only when dtach is installed with the dtach backend, never with none.
func Available() bool {
	switch Backend() {
	case BackendNative:
		return true
	case BackendDtach:
		_, ok := Path()
		return ok
	}
	return false
}

// Sock is the per-session holder socket on this host, derived from the session
// id so any viewer reattaches the same held process by id. The id is hashed to
// a short, fixed-length filename to stay well under the unix-socket path limit
// (~104 bytes), which a full id under a long home path could otherwise exceed.
// It ensures the parent dir exists, preferring $XDG_RUNTIME_DIR when set. Both
// the native and dtach backends key their socket here, so switching backends
// never strands a session's address.
func Sock(id string) string {
	var d string
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" {
		d = filepath.Join(base, "ax", "run")
		os.MkdirAll(d, 0o700) // sockets are session control; keep them private
	} else {
		d = axdir.State("run")
	}
	return filepath.Join(d, "ax-"+idHash(id)+".sock")
}

// idHash is the short session-id hash both endpoint namings share: the unix
// socket filename (kept under the ~104-byte path limit) and the Windows pipe
// name, so an attach by id lands on the same holder on either platform.
func idHash(id string) string {
	sum := sha1.Sum([]byte(id))
	return hex.EncodeToString(sum[:8])
}

// pipeName is the per-session holder endpoint on Windows: a named pipe in the
// global pipe namespace, derived from the session id exactly as Sock is. Pipes
// need no directory, no permission bits (the listener ACLs it to the owner),
// and no cleanup (the name vanishes with the last handle).
func pipeName(id string) string {
	return `\\.\pipe\ax-` + idHash(id)
}

// Cleanup removes a session's holder socket, for teardown paths (restart
// --fresh) clearing a stale endpoint. Best-effort.
func Cleanup(id string) { os.Remove(Sock(id)) }
