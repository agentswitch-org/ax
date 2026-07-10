// Package search finds a query inside transcript text. Searcher is the contract:
// ripgrep when it is installed (fast), else an in-process line scan, so content
// search works with no external tool. Both do a case-insensitive literal match.
package search

import (
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/agentswitch-org/ax/internal/config"
)

// Searcher searches files for a query.
type Searcher interface {
	// Matches returns, for each file with a hit, its matching line numbers
	// (1-based). perFile caps results per file; 0 means no cap.
	Matches(query string, files []string, perFile int) map[string][]int
}

// New returns ripgrep when it is on PATH, else the in-process searcher.
func New(cfg config.Config) Searcher {
	if _, err := exec.LookPath("rg"); err == nil {
		return ripgrep{}
	}
	return inproc{}
}

// inproc scans each file for the query in-process, for when ripgrep is absent.
type inproc struct{}

func (inproc) Matches(query string, files []string, perFile int) map[string][]int {
	query = strings.TrimSpace(query)
	if query == "" || len(files) == 0 {
		return nil
	}
	q := strings.ToLower(query)
	res := map[string][]int{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		hits := 0
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(strings.ToLower(line), q) {
				res[f] = append(res[f], i+1)
				if hits++; perFile > 0 && hits >= perFile {
					break
				}
			}
		}
	}
	return res
}

type ripgrep struct{}

func (ripgrep) Matches(query string, files []string, perFile int) map[string][]int {
	query = strings.TrimSpace(query)
	if query == "" || len(files) == 0 {
		return nil
	}
	args := []string{"-n", "-H", "-i", "-F"}
	if perFile > 0 {
		args = append(args, "-m", strconv.Itoa(perFile))
	}
	args = append(args, "--", query)
	args = append(args, files...)
	out, _ := exec.Command("rg", args...).Output()
	res := map[string][]int{}
	for _, m := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if file, line, ok := splitMatch(m); ok {
			res[file] = append(res[file], line)
		}
	}
	return res
}

// splitMatch parses ripgrep's "path:line:text". Paths here are absolute with no
// colons, so the first two colons delimit the fields.
func splitMatch(m string) (file string, line int, ok bool) {
	a := strings.IndexByte(m, ':')
	if a < 0 {
		return "", 0, false
	}
	b := strings.IndexByte(m[a+1:], ':')
	if b < 0 {
		return "", 0, false
	}
	b += a + 1
	n, err := strconv.Atoi(m[a+1 : b])
	if err != nil {
		return "", 0, false
	}
	return m[:a], n, true
}
