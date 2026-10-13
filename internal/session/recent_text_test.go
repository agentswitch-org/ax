package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecentTextKeepsLastTurnsInOrder(t *testing.T) {
	for _, format := range []string{"claude", "codex", "pi"} {
		t.Run(format, func(t *testing.T) {
			var data strings.Builder
			for i := 0; i < 25; i++ {
				content := fmt.Sprintf(`[{"type":"text","text":"turn %d"},{"type":"tool_use"},{"type":"text","text":"second block"}]`, i)
				switch format {
				case "claude":
					fmt.Fprintf(&data, "{\"type\":\"user\",\"message\":{\"content\":%s}}\n", content)
				case "pi":
					fmt.Fprintf(&data, "{\"type\":\"message\",\"message\":{\"role\":\"user\",\"content\":%s}}\n", content)
				case "codex":
					fmt.Fprintf(&data, "{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":%s}}\n", content)
				}
			}
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			if err := os.WriteFile(path, []byte(data.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, count := range []int{-1, 0, 1, 12, 25, 30} {
				got := RecentText(format, path, count)
				wantLen := max(0, min(count, 25))
				if len(got) != wantLen {
					t.Fatalf("count %d: got %d turns", count, len(got))
				}
				for i, turn := range got {
					want := fmt.Sprintf("turn %d\nsecond block", 25-wantLen+i)
					if turn.Text != want {
						t.Fatalf("count %d, turn %d: got %q, want %q", count, i, turn.Text, want)
					}
				}
			}
			full := FullText(format, path)
			if !strings.HasPrefix(full, "turn 0\nsecond block\n") || strings.Count(full, "second block") != 25 {
				t.Fatalf("FullText lost message blocks: %q", full)
			}
		})
	}
}
