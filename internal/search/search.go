// Package search finds a query inside transcript text. Searcher is the contract:
// ripgrep when it is installed (fast), else an in-process line scan, so content
// search works with no external tool. Both do a case-insensitive literal match.
package search

import (
	"bufio"
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
		file, err := os.Open(f)
		if err != nil {
			continue
		}
		hits := 0
		reader := bufio.NewReader(file)
		for i := 1; ; i++ {
			line, err := reader.ReadString('\n')
			if strings.Contains(strings.ToLower(line), q) {
				res[f] = append(res[f], i)
				if hits++; perFile > 0 && hits >= perFile {
					break
				}
			}
			if err != nil {
				break
			}
		}
		file.Close()
	}
	return res
}

type ripgrep struct{}

func (ripgrep) Matches(query string, files []string, perFile int) map[string][]int {
	query = strings.TrimSpace(query)
	if query == "" || len(files) == 0 {
		return nil
	}
	args := []string{"-n", "-H", "-i", "-F", "--null"}
	if perFile > 0 {
		args = append(args, "-m", strconv.Itoa(perFile))
	}
	args = append(args, "--", query)
	args = append(args, files...)
	out, _ := exec.Command("rg", args...).Output()
	res := map[string][]int{}
	rest := string(out)
	for rest != "" {
		// A NUL separates the path from line:text; unlike a colon/newline it
		// cannot occur in a filename (including a Windows drive path).
		path, body, ok := strings.Cut(rest, "\x00")
		if !ok {
			break
		}
		record, tail, _ := strings.Cut(body, "\n")
		rest = tail
		if file, line, ok := splitMatch(path + "\x00" + record); ok {
			res[file] = append(res[file], line)
		}
	}
	return res
}

// splitMatch parses ripgrep's "path<NUL>line:text". The colon-delimited form
// remains accepted for older callers, but --null keeps real paths unambiguous.
func splitMatch(m string) (file string, line int, ok bool) {
	a := strings.IndexByte(m, 0)
	if a < 0 {
		a = strings.IndexByte(m, ':')
	}
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
