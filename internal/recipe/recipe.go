package recipe

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Recipe is a user-owned recipe file plus the cosmetic metadata and argv prefix
// ax will use when launching it. The caller appends Path to Interp at exec time.
type Recipe struct {
	Path        string
	Name        string
	Description string
	Interp      []string
}

// List returns every directly contained regular recipe file in dir. Recipe
// files are not filtered by extension or host compatibility; dotfiles and
// editor backup files ending in "~" are the only name-based skips.
func List(dir string) ([]Recipe, error) {
	if dir == "" {
		return []Recipe{}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Recipe{}, nil
		}
		return []Recipe{}, fmt.Errorf("list recipes in %q: %w", dir, err)
	}

	out := make([]Recipe, 0, len(entries))
	for _, entry := range entries {
		base := entry.Name()
		if base == "" || strings.HasPrefix(base, ".") || strings.HasSuffix(base, "~") || entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return out, fmt.Errorf("stat recipe %q: %w", filepath.Join(dir, base), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}

		path := filepath.Join(dir, base)
		name := strings.TrimSuffix(base, filepath.Ext(base))
		description := ""
		if content, err := os.ReadFile(path); err == nil {
			if headerName, headerDescription := ParseHeader(content); headerName != "" || headerDescription != "" {
				if headerName != "" {
					name = headerName
				}
				description = headerDescription
			}
		}
		interp, err := Interpreter(path)
		if err != nil {
			return out, err
		}
		out = append(out, Recipe{
			Path:        path,
			Name:        name,
			Description: description,
			Interp:      interp,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		iName, jName := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if iName != jName {
			return iName < jName
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// Interpreter resolves the argv prefix used to execute path. It performs no
// availability or compatibility checks; missing interpreters fail naturally when
// the caller executes Interp + []string{path}.
func Interpreter(path string) ([]string, error) {
	line, _ := firstLine(path)
	return interpreterFor(path, line, runtime.GOOS), nil
}

// ParseHeader reads leading "# ax: key = value" comments. Only name and
// description are recognized; unknown keys remain cosmetic and are ignored.
func ParseHeader(content []byte) (name, description string) {
	for _, raw := range bytes.Split(content, []byte{'\n'}) {
		line := strings.TrimRight(string(raw), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		line = strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(line, "#") {
			break
		}
		body := strings.TrimLeft(strings.TrimPrefix(line, "#"), " \t")
		if !strings.HasPrefix(body, "ax:") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(body, "ax:")), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "name":
			name = strings.TrimSpace(value)
		case "description":
			description = strings.TrimSpace(value)
		}
	}
	return name, description
}

func firstLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func interpreterFor(path, first string, goos string) []string {
	if goos != "windows" && strings.HasPrefix(first, "#!") {
		if argv := strings.Fields(strings.TrimSpace(strings.TrimPrefix(first, "#!"))); len(argv) > 0 {
			return argv
		}
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".ps1":
		return []string{"pwsh", "-NoProfile", "-File"}
	case ".sh", ".bash":
		return []string{"bash"}
	case ".cmd", ".bat":
		if goos == "windows" {
			return []string{"cmd", "/c"}
		}
	}
	return hostDefault(goos)
}

func hostDefault(goos string) []string {
	if goos == "windows" {
		return []string{"pwsh", "-NoProfile", "-File"}
	}
	// Run the recipe as a script (`sh <path>`), not as a command string
	// (`sh -c <path>`). -c would execute the path itself as the command, which
	// only works for a +x, self-describing file; passing the path as sh's script
	// argument matches `bash <path>` for .sh and runs any no-shebang recipe.
	return []string{"sh"}
}
