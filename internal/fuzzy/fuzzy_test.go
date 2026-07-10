package fuzzy

import (
	"testing"
)

func TestRankEmptyQueryReturnsAll(t *testing.T) {
	items := []string{"foo", "bar", "baz"}
	got := Rank("", items)
	if len(got) != len(items) {
		t.Fatalf("want %d indices, got %d", len(items), len(got))
	}
	for i, v := range got {
		if v != i {
			t.Errorf("index %d: want %d, got %d", i, i, v)
		}
	}
}

func TestRankEmptyQueryEmptyItems(t *testing.T) {
	got := Rank("", nil)
	if len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
}

func TestRankNoMatch(t *testing.T) {
	got := Rank("xyz", []string{"abc", "def", "ghi"})
	if len(got) != 0 {
		t.Fatalf("want no matches, got %v", got)
	}
}

func TestRankPartialFilter(t *testing.T) {
	// "ab" matches "ab" and "axb" but not "cd"
	items := []string{"ab", "cd", "axb"}
	got := Rank("ab", items)
	if len(got) != 2 {
		t.Fatalf("want 2 matches, got %v", got)
	}
	inResult := map[int]bool{}
	for _, v := range got {
		inResult[v] = true
	}
	if !inResult[0] {
		t.Error("index 0 (\"ab\") should match")
	}
	if inResult[1] {
		t.Error("index 1 (\"cd\") should not match")
	}
	if !inResult[2] {
		t.Error("index 2 (\"axb\") should match")
	}
}

func TestRankExactLengthMatchFirst(t *testing.T) {
	// n==m triggers math.Inf(1), so "foo" ranks first among these
	items := []string{"foobar", "foo", "xfoo"}
	got := Rank("foo", items)
	if len(got) == 0 {
		t.Fatal("expected matches")
	}
	if got[0] != 1 {
		t.Errorf("exact-length match should rank first; got first index %d", got[0])
	}
}

// Smart case: lowercase query -> case insensitive.
func TestRankSmartCaseLowerQueryCaseInsensitive(t *testing.T) {
	items := []string{"Foo", "FOO", "foo", "bar"}
	got := Rank("foo", items)
	// All three f-cases should match.
	if len(got) != 3 {
		t.Fatalf("want 3 matches, got %d (%v)", len(got), got)
	}
	inResult := map[int]bool{}
	for _, v := range got {
		inResult[v] = true
	}
	for _, want := range []int{0, 1, 2} {
		if !inResult[want] {
			t.Errorf("index %d should match in case-insensitive mode", want)
		}
	}
	if inResult[3] {
		t.Error("\"bar\" should not match \"foo\"")
	}
}

// Smart case: query with uppercase -> case sensitive.
func TestRankSmartCaseUpperQueryCaseSensitive(t *testing.T) {
	items := []string{"foo", "FOO", "Foo"}
	got := Rank("Foo", items)
	if len(got) != 1 {
		t.Fatalf("want 1 match (case-sensitive), got %d (%v)", len(got), got)
	}
	if got[0] != 2 {
		t.Errorf("want index 2 (\"Foo\"), got %d", got[0])
	}
}

// Word-boundary match (after '-') ranks above an inner consecutive match.
func TestRankWordBoundaryBeatsInnerConsecutive(t *testing.T) {
	// "a-bcd": 'a' at 0 (slash-start bonus 0.9), 'b' at 2 (word-boundary bonus 0.8) = 1.69
	// "xabc":  'a' at 1 (no bonus), 'b' at 2 (consecutive bonus 1.0) = ~0.99
	items := []string{"a-bcd", "xabc"}
	got := Rank("ab", items)
	if len(got) != 2 {
		t.Fatalf("both should match, got %v", got)
	}
	if got[0] != 0 {
		t.Errorf("word-boundary item should rank first, got index %d first", got[0])
	}
}

// Slash boundary: path item ranks above a non-boundary item.
func TestRankSlashBoundaryFirst(t *testing.T) {
	// "src/main.go": 's' at 0 gets slash bonus, total score high
	// "mysrcfile": 's' at 2, no boundary bonus
	items := []string{"mysrcfile", "src/main.go"}
	got := Rank("src", items)
	if len(got) != 2 {
		t.Fatalf("both should match, got %v", got)
	}
	if got[0] != 1 {
		t.Errorf("path item (index 1) should rank first, got %d", got[0])
	}
}

// CamelCase capital bonus: 'B' following lowercase scores higher.
func TestRankCapitalBoundaryBonus(t *testing.T) {
	// query "fb" case-insensitive
	// "FooBar": 'f' at 0 (slash bonus 0.9), 'B' at 3 (capital bonus 0.7) = better
	// "foobar": 'f' at 0 (slash bonus 0.9), 'b' at 3 (no bonus)
	items := []string{"foobar", "FooBar"}
	got := Rank("fb", items)
	if len(got) != 2 {
		t.Fatalf("both should match, got %v", got)
	}
	if got[0] != 1 {
		t.Errorf("CamelCase item (index 1) should rank first, got %d", got[0])
	}
}

// Consecutive-match bonus: "ab" adjacent beats "ab" with a gap.
func TestRankConsecutiveMatchBonus(t *testing.T) {
	// "ab_cd": 'a' at 0 (slash bonus), 'b' at 1 (consecutive bonus) -> very high
	// "axb":   'a' at 0 (slash bonus), 'b' at 2 (no consecutive, no boundary)
	items := []string{"axb", "ab_cd"}
	got := Rank("ab", items)
	if len(got) != 2 {
		t.Fatalf("both should match, got %v", got)
	}
	if got[0] != 1 {
		t.Errorf("consecutive item (index 1) should rank first, got %d", got[0])
	}
}

// Dot boundary bonus.
func TestRankDotBoundaryBonus(t *testing.T) {
	// "foo.bar": 'b' at 4 gets dot bonus (scoreMatchDot=0.6)
	// "xbarfoo": 'b' at 1, no bonus
	// Both match query "b"; "foo.bar" should win on dot bonus.
	items := []string{"xbarfoo", "foo.bar"}
	got := Rank("b", items)
	if len(got) != 2 {
		t.Fatalf("both should match query \"b\", got %v", got)
	}
	if got[0] != 1 {
		t.Errorf("dot-boundary item (index 1) should rank first, got %d", got[0])
	}
}

// Multiple items: verify stable ordering is preserved for ties at same score.
func TestRankMultipleMatchesOrdered(t *testing.T) {
	items := []string{"alpha", "beta", "gamma", "delta"}
	got := Rank("a", items)
	// "alpha"=0, "beta"=1, "gamma"=2, "delta"=3 all contain 'a'
	if len(got) != 4 {
		t.Fatalf("all items contain 'a', got %v", got)
	}
}

// Single character query matches only items containing that character.
func TestRankSingleCharQuery(t *testing.T) {
	items := []string{"xyz", "abc", "mno"}
	got := Rank("a", items)
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("want [1], got %v", got)
	}
}

// hasMatch: subsequence check.
func TestHasMatch(t *testing.T) {
	tests := []struct {
		q, t string
		fold bool
		want bool
	}{
		{"abc", "abcde", true, true},
		{"abc", "axbxcx", true, true},
		{"abc", "ab", true, false}, // query longer than text
		{"abc", "xyz", true, false},
		{"ABC", "abc", false, false}, // case-sensitive miss
		{"ABC", "abc", true, true},   // case-insensitive hit
		{"", "abc", true, true},      // empty query always matches
	}
	for _, tc := range tests {
		got := hasMatch([]rune(tc.q), []rune(tc.t), tc.fold)
		if got != tc.want {
			t.Errorf("hasMatch(%q, %q, fold=%v) = %v, want %v", tc.q, tc.t, tc.fold, got, tc.want)
		}
	}
}

// charBonus: verify each boundary type.
func TestCharBonus(t *testing.T) {
	tests := []struct {
		prev, cur rune
		want      float64
	}{
		{'/', 'a', scoreMatchSlash},
		{'-', 'a', scoreMatchWord},
		{'_', 'a', scoreMatchWord},
		{' ', 'a', scoreMatchWord},
		{'.', 'a', scoreMatchDot},
		{'a', 'B', scoreMatchCapital}, // lower->upper transition
		{'a', 'b', 0},                 // no bonus
		{'A', 'B', 0},                 // upper->upper: not a camelCase transition
	}
	for _, tc := range tests {
		got := charBonus(tc.prev, tc.cur)
		if got != tc.want {
			t.Errorf("charBonus(%q, %q) = %v, want %v", tc.prev, tc.cur, got, tc.want)
		}
	}
}

// hasUpper tests.
func TestHasUpper(t *testing.T) {
	if hasUpper([]rune("abc")) {
		t.Error("\"abc\" should not have uppercase")
	}
	if !hasUpper([]rune("Abc")) {
		t.Error("\"Abc\" should have uppercase")
	}
	if !hasUpper([]rune("ABC")) {
		t.Error("\"ABC\" should have uppercase")
	}
	if hasUpper([]rune("")) {
		t.Error("empty string should not have uppercase")
	}
}
