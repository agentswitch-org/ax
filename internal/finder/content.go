package finder

import (
	"os"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/view"
)

func (p *picker) contentMode() bool {
	return p.mode == mContent || (p.mode == mNormal && p.committed == mContent)
}

// Only one local scan may run at a time. If typing or a refresh overtakes it,
// coalesce the pending work into one scan of the latest query and session set.
func (p *picker) kickLocalSearch() {
	if !p.contentMode() || strings.TrimSpace(p.query) == "" || p.searchReady == nil {
		return
	}
	if p.localSearchInFlight {
		p.localSearchAgain = true
		return
	}
	p.localSearchInFlight = true
	p.localSearchAgain = false
	q, cfg, searcher := strings.TrimSpace(p.query), p.cfg, p.searcher
	all := append([]session.Session(nil), p.all...)
	go func() {
		hits := make(map[string]bool)
		// Bound command-line length and transient memory for large histories.
		for start := 0; start < len(all); start += 64 {
			select {
			case <-p.done:
				return
			default:
			}
			files := make([]string, 0, 64)
			keys := make([]string, 0, 64)
			for _, s := range all[start:min(start+64, len(all))] {
				if s.Host != "" {
					continue
				}
				if f := view.TextFile(cfg, s); f != "" {
					files = append(files, f)
					keys = append(keys, session.Key(s))
				}
			}
			if len(files) == 0 {
				continue
			}
			matches := searcher.Matches(q, files, 1)
			for i, f := range files {
				if len(matches[f]) > 0 {
					hits[keys[i]] = true
				}
			}
		}
		select {
		case p.searchReady <- searchResult{query: q, hits: hits, local: true}:
		case <-p.done:
		}
	}()
}

func (p *picker) applySearchResult(sr searchResult) {
	if sr.local {
		p.localSearchInFlight = false
	}
	current := p.contentMode() && sr.query == strings.TrimSpace(p.query)
	if current {
		if sr.local {
			p.localHits, p.localQuery = sr.hits, sr.query
		} else {
			p.remoteHits, p.remoteQuery = sr.hits, sr.query
		}
		p.recompute()
		p.previewDirty = true
	}
	if sr.local && (!current || p.localSearchAgain) {
		p.kickLocalSearch()
	}
}

type contentPreviewResult struct {
	key, query, file string
	last             time.Time
	lines            []string
	matches          []int
}

// Keep only the most recent content preview, with one render in flight. A slow
// transcript cannot spawn a new reader for every cursor movement or keystroke.
func (p *picker) buildContentPreview(s session.Session, query string) {
	key := session.Key(s)
	if cached := p.contentPreview; cached != nil && cached.key == key && cached.query == query && cached.file == s.File && cached.last.Equal(s.Last) {
		p.preview, p.matchLines = cached.lines, cached.matches
		if len(p.matchLines) > 0 {
			p.previewTop = max(p.matchLines[0]-2, 0)
		}
		return
	}
	p.preview = []string{"", "  loading search preview …"}
	p.previewTop = 0
	if p.contentInFlight || p.contentReady == nil {
		return
	}
	p.contentInFlight = true
	cfg, remotePreview := p.cfg, p.remotePreview
	go func() {
		result := contentPreviewResult{key: key, query: query, file: s.File, last: s.Last}
		if s.Host != "" && remotePreview != nil {
			result.lines = remotePreview(s)
		} else {
			data, _ := os.ReadFile(view.TextFile(cfg, s))
			result.lines = strings.Split(string(data), "\n")
		}
		q := strings.ToLower(query)
		for i, line := range result.lines {
			line = expandTabs(line)
			result.lines[i] = view.Highlight(line, query)
			if strings.Contains(strings.ToLower(line), q) {
				result.matches = append(result.matches, i)
			}
		}
		select {
		case p.contentReady <- result:
		case <-p.done:
		}
	}()
}
