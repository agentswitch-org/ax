package app

import "testing"

// RunEnv prefers AX_RUN and falls back to the deprecated AX_GROUP, so a
// process that only ever set the old name still reads its run id.
func TestRunEnvPrefersRunOverDeprecatedGroup(t *testing.T) {
	t.Setenv("AX_RUN", "")
	t.Setenv("AX_GROUP", "")
	if got := RunEnv(); got != "" {
		t.Fatalf("RunEnv() = %q, want empty", got)
	}

	t.Setenv("AX_GROUP", "fromgroup")
	if got := RunEnv(); got != "fromgroup" {
		t.Fatalf("RunEnv() = %q, want fallback to AX_GROUP", got)
	}

	t.Setenv("AX_RUN", "fromrun")
	if got := RunEnv(); got != "fromrun" {
		t.Fatalf("RunEnv() = %q, want AX_RUN to win over AX_GROUP", got)
	}
}
