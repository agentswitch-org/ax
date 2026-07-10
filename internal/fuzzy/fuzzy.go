// Package fuzzy is an in-process fuzzy matcher, so ax does not shell out to fzf
// on every keystroke (one fewer runtime dependency, and no subprocess per key).
// It uses the fzy scoring algorithm: a query matches a string as a subsequence,
// scored with bonuses for matches at word boundaries and for consecutive runs,
// and gap penalties, so the best matches rank first.
package fuzzy

import (
	"math"
	"sort"
	"unicode"
)

const (
	scoreGapLeading       = -0.005
	scoreGapTrailing      = -0.005
	scoreGapInner         = -0.01
	scoreMatchConsecutive = 1.0
	scoreMatchSlash       = 0.9
	scoreMatchWord        = 0.8
	scoreMatchCapital     = 0.7
	scoreMatchDot         = 0.6
)

var scoreMin = math.Inf(-1)

// Rank returns the indices of items that match query, best score first. An empty
// query keeps every item in its original order. Matching is smart-case:
// case-insensitive unless the query has an uppercase letter.
func Rank(query string, items []string) []int {
	q := []rune(query)
	if len(q) == 0 {
		out := make([]int, len(items))
		for i := range items {
			out[i] = i
		}
		return out
	}
	fold := !hasUpper(q)
	type scored struct {
		idx   int
		score float64
	}
	var hits []scored
	for i, it := range items {
		t := []rune(it)
		if !hasMatch(q, t, fold) {
			continue
		}
		hits = append(hits, scored{i, score(q, t, fold)})
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	out := make([]int, len(hits))
	for i, h := range hits {
		out[i] = h.idx
	}
	return out
}

func hasUpper(rs []rune) bool {
	for _, r := range rs {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

func eq(a, b rune, fold bool) bool {
	if fold {
		return unicode.ToLower(a) == unicode.ToLower(b)
	}
	return a == b
}

// hasMatch reports whether query is a subsequence of text (a cheap reject before
// the scoring DP).
func hasMatch(query, text []rune, fold bool) bool {
	i := 0
	for j := 0; j < len(text) && i < len(query); j++ {
		if eq(query[i], text[j], fold) {
			i++
		}
	}
	return i == len(query)
}

// score runs the fzy DP and returns the best alignment score. It assumes query
// matches text (call hasMatch first).
func score(query, text []rune, fold bool) float64 {
	n, m := len(query), len(text)
	if n == m {
		return math.Inf(1) // exact-length subsequence match: perfect
	}
	bonus := precomputeBonus(text)

	// D[j]: best score for the current query char ending at text[j].
	// M[j]: best score for query[:i+1] using text[:j+1] (last char at or before j).
	D := make([]float64, m)
	M := make([]float64, m)
	var prevD, prevM []float64
	for i := 0; i < n; i++ {
		prevScore := scoreMin
		gap := scoreGapInner
		if i == n-1 {
			gap = scoreGapTrailing
		}
		for j := 0; j < m; j++ {
			if eq(query[i], text[j], fold) {
				s := scoreMin
				if i == 0 {
					s = float64(j)*scoreGapLeading + bonus[j]
				} else if j > 0 {
					s = math.Max(prevM[j-1]+bonus[j], prevD[j-1]+scoreMatchConsecutive)
				}
				D[j] = s
				prevScore = math.Max(s, prevScore+gap)
				M[j] = prevScore
			} else {
				D[j] = scoreMin
				prevScore = prevScore + gap
				M[j] = prevScore
			}
		}
		prevD, prevM = D, M
		D, M = make([]float64, m), make([]float64, m)
	}
	return prevM[m-1]
}

// precomputeBonus scores each position by the character preceding it, so matches
// at word starts (after a slash, separator, dot, or at a camelCase boundary) rank
// higher.
func precomputeBonus(text []rune) []float64 {
	bonus := make([]float64, len(text))
	prev := '/'
	for i, r := range text {
		bonus[i] = charBonus(prev, r)
		prev = r
	}
	return bonus
}

func charBonus(prev, cur rune) float64 {
	switch prev {
	case '/':
		return scoreMatchSlash
	case '-', '_', ' ':
		return scoreMatchWord
	case '.':
		return scoreMatchDot
	}
	if unicode.IsLower(prev) && unicode.IsUpper(cur) {
		return scoreMatchCapital
	}
	return 0
}
