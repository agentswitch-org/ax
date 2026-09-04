package app

// configstores.go implements `ax config stores`: a read-only, local view of
// every transcript store ax indexes per harness, so "why don't my sessions show
// up?" has a direct answer. It lists each store's role (primary / a foreign
// win/wsl store), where it came from (config, an env override, WSL auto-
// discovery), its path, and how many transcripts currently match. It also warns
// when a harness binary resolves across the WSL interop boundary, the exact case
// where hand-started sessions land in the Windows store instead of the WSL one.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
)

// configStores prints the resolved store list for every configured harness.
func (a App) configStores(_ []string) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax config stores: %v\n", err)
		os.Exit(1)
	}

	if config.InWSL() {
		fmt.Printf("running in WSL; Windows stores are auto-indexed under %s\n", config.MountRoot())
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "HARNESS\tROLE\tSOURCE\tMATCHES\tNEWEST\tPATH")
	for _, h := range cfg.Harnesses {
		stores := h.EffectiveStores()
		if len(stores) == 0 {
			fmt.Fprintf(w, "%s\t-\t-\t-\t-\t(no store configured)\n", h.Name)
			continue
		}
		for _, st := range stores {
			role := "secondary"
			if st.Primary {
				role = "primary"
			}
			if st.Label != "" {
				role = st.Label // "win" / "wsl": a foreign store, never primary
			}
			n, newest := countStore(st)
			path := st.Glob
			if path == "" {
				path = st.DB
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", h.Name, role, st.Source, n, ageOrDash(newest), path)
		}
	}
	w.Flush()

	warnInteropHarnesses(cfg)
}

// countStore returns how many transcripts a store currently matches and the
// newest modification time among them. For a db store it is 1/mtime when the
// file exists, 0 otherwise.
func countStore(st config.Store) (int, time.Time) {
	if st.DB != "" {
		if fi, err := os.Stat(st.DB); err == nil {
			return 1, fi.ModTime()
		}
		return 0, time.Time{}
	}
	matches, _ := filepath.Glob(st.Glob)
	var newest time.Time
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return len(matches), newest
}

// warnInteropHarnesses prints a warning for each harness whose binary resolves
// under the WSL mount root: the harness is the Windows install reached through
// interop, so sessions the user starts "in WSL" are filed in the Windows store.
// ax indexes those stores, so the sessions still show; the note explains the
// win badge and points at the native-Linux install for same-store resume.
func warnInteropHarnesses(cfg config.Config) {
	root := config.MountRoot()
	if root == "" {
		return
	}
	for _, h := range cfg.Harnesses {
		p, err := exec.LookPath(h.Name)
		if err != nil {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if strings.HasPrefix(p, root) {
			fmt.Fprintf(os.Stderr, "note: %q resolves to a Windows binary through interop (%s); its sessions file in the Windows store and show with a ·win badge. Install the native Linux %s in WSL for same-store resume.\n", h.Name, p, h.Name)
		}
	}
}

// ageOrDash renders a store's newest-transcript age, or "-" when it has none.
func ageOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
