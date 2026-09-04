package config

// WSL crosses a filesystem boundary that ax has to reach across: a `claude` on
// the WSL PATH resolves to the Windows install through interop and files
// sessions in the Windows home under /mnt/c/Users/<you>/.claude, so ax running
// in WSL must index the Windows stores too, and translate a recorded Windows cwd
// (C:\Users\you\src) into its WSL mount form (/mnt/c/Users/you/src). The reverse
// direction (native Windows reaching a running distro's home over \\wsl.localhost)
// is handled best-effort.

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// InWSL reports whether this process runs inside a WSL distribution.
func InWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	for _, p := range []string{"/proc/sys/kernel/osrelease", "/proc/version"} {
		if b, err := os.ReadFile(p); err == nil {
			l := strings.ToLower(string(b))
			if strings.Contains(l, "microsoft") || strings.Contains(l, "wsl") {
				return true
			}
		}
	}
	return false
}

// wslMountRoot is where WSL automounts the Windows drives, "/mnt/" by default,
// overridable by [automount] root in /etc/wsl.conf. Always ends in "/".
func wslMountRoot() string {
	root := "/mnt/"
	if f, err := os.Open("/etc/wsl.conf"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		section := ""
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				section = strings.ToLower(strings.Trim(line, "[]"))
				continue
			}
			if section == "automount" && strings.HasPrefix(line, "root") {
				if i := strings.IndexByte(line, '='); i >= 0 {
					if v := strings.TrimSpace(line[i+1:]); v != "" {
						root = v
					}
				}
			}
		}
	}
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}
	return root
}

// windowsHomesFromWSL returns the Windows user home directories visible from
// inside WSL, e.g. /mnt/c/Users/alice. Service accounts are skipped.
func windowsHomesFromWSL() []string {
	return windowsHomesUnder(wslMountRoot())
}

// windowsHomesUnder enumerates <root><drive>/Users/* home directories, split out
// from wslMountRoot for testing against a fake mount tree.
func windowsHomesUnder(root string) []string {
	var homes []string
	drives, _ := filepath.Glob(root + "*")
	for _, d := range drives {
		users := filepath.Join(d, "Users")
		if fi, err := os.Stat(users); err != nil || !fi.IsDir() {
			continue
		}
		ents, _ := os.ReadDir(users)
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			switch strings.ToLower(e.Name()) {
			case "public", "default", "default user", "all users":
				continue
			}
			homes = append(homes, filepath.Join(users, e.Name()))
		}
	}
	return homes
}

// wslHomesFromWindows returns the per-user home directories of the running WSL
// distributions, reached over the \\wsl.localhost UNC path, best-effort. Empty
// unless this is native Windows with `wsl.exe` available.
func wslHomesFromWindows() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	out, err := exec.Command("wsl.exe", "-l", "-q").Output()
	if err != nil {
		return nil
	}
	var homes []string
	for _, distro := range parseWSLList(out) {
		usersRoot := `\\wsl.localhost\` + distro + `\home`
		ents, err := os.ReadDir(usersRoot)
		if err != nil {
			// Older builds expose the share as \\wsl$\<distro>.
			usersRoot = `\\wsl$\` + distro + `\home`
			if ents, err = os.ReadDir(usersRoot); err != nil {
				continue
			}
		}
		for _, e := range ents {
			if e.IsDir() {
				homes = append(homes, filepath.Join(usersRoot, e.Name()))
			}
		}
	}
	return homes
}

// parseWSLList decodes the distro names from `wsl.exe -l -q` output, which is
// UTF-16LE. The ASCII payload is every other byte, so dropping the NUL bytes
// recovers the names without a UTF-16 dependency.
func parseWSLList(raw []byte) []string {
	b := make([]byte, 0, len(raw))
	for _, c := range raw {
		if c != 0 && c != '\r' {
			b = append(b, c)
		}
	}
	var names []string
	for _, line := range strings.Split(string(b), "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// winToUnixWSL maps a Windows path to its WSL mount form: C:\Users\a\src becomes
// <root>c/Users/a/src (root is "/mnt/" by default). A path that is not a
// drive-letter path is returned unchanged.
func winToUnixWSL(p, root string) string {
	p = strings.TrimSpace(p)
	if root == "" {
		root = "/mnt/"
	}
	if len(p) >= 2 && p[1] == ':' && isDriveLetter(p[0]) {
		drive := strings.ToLower(p[:1])
		rest := strings.ReplaceAll(p[2:], `\`, "/")
		if !strings.HasPrefix(rest, "/") {
			rest = "/" + rest
		}
		return root + drive + rest
	}
	return p
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
