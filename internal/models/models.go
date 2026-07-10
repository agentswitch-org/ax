// Package models resolves a model id to its context window and prices, using a
// snapshot from models.dev. A refreshed copy in the state dir is preferred over
// the binary's embedded fallback.
package models

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
)

//go:embed models.json
var embedded []byte

// Info holds the per-model facts the transcripts don't carry: context size and
// prices (USD per million tokens).
type Info struct {
	Context    int     `json:"context"`
	OutputLim  int     `json:"output_limit"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// DB is a flat model-id -> Info map.
type DB map[string]Info

// Load prefers a refreshed snapshot in the state dir, falling back to the
// snapshot embedded in the binary.
func Load() DB {
	if data, err := os.ReadFile(statePath()); err == nil {
		var db DB
		if json.Unmarshal(data, &db) == nil && len(db) > 0 {
			return db
		}
	}
	var db DB
	json.Unmarshal(embedded, &db)
	if db == nil {
		db = DB{}
	}
	return db
}

// Lookup returns the Info for a model id.
func (db DB) Lookup(model string) (Info, bool) {
	i, ok := db[model]
	return i, ok
}

func statePath() string {
	return axdir.StatePath("models.json")
}

// Update fetches the latest models.dev catalog, slims it to the fields ax needs
// (first-party providers win over resellers), and writes it to the state dir.
func Update() (int, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get("https://models.dev/api.json")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var raw map[string]struct {
		Models map[string]struct {
			Limit struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			Cost struct {
				Input      float64 `json:"input"`
				Output     float64 `json:"output"`
				CacheRead  float64 `json:"cache_read"`
				CacheWrite float64 `json:"cache_write"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, err
	}

	priority := []string{"anthropic", "openai", "google", "xai", "mistral",
		"deepseek", "cohere", "llama", "moonshotai", "minimax", "zhipuai",
		"zai", "stepfun", "alibaba", "perplexity", "nvidia"}
	seen := map[string]bool{}
	order := append([]string{}, priority...)
	rest := make([]string, 0, len(raw))
	for p := range raw {
		if !contains(priority, p) {
			rest = append(rest, p)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)

	db := DB{}
	for _, prov := range order {
		pv, ok := raw[prov]
		if !ok {
			continue
		}
		for mid, mv := range pv.Models {
			if seen[mid] {
				continue
			}
			seen[mid] = true
			db[mid] = Info{
				Context:    mv.Limit.Context,
				OutputLim:  mv.Limit.Output,
				Input:      mv.Cost.Input,
				Output:     mv.Cost.Output,
				CacheRead:  mv.Cost.CacheRead,
				CacheWrite: mv.Cost.CacheWrite,
			}
		}
	}

	out, err := json.Marshal(db)
	if err != nil {
		return 0, err
	}
	p := statePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return 0, err
	}
	if err := os.WriteFile(p, out, 0o600); err != nil {
		return 0, err
	}
	return len(db), nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
