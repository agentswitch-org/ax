package fuzzy

import (
	"fmt"
	"testing"
)

// Model the all-fields picker search: session identity, project, harness, and
// task text, including the longer queries users type to narrow a large library.
func BenchmarkRankSessions(b *testing.B) {
	for _, n := range []int{100, 500, 2000} {
		items := make([]string, n)
		for i := range items {
			items[i] = fmt.Sprintf("claude worker-%d /src/project-%d refactor the parser and add regression coverage session-%06d", i, i%50, i)
		}
		for _, query := range []string{"parser", "regression"} {
			b.Run(fmt.Sprintf("%d/%s", n, query), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					Rank(query, items)
				}
			})
		}
	}
}
