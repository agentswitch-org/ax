package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// App tests exercise discovery as well as commands. A temporary state directory
// alone still leaves default transcript stores pointing at the developer's home.
// Start the whole suite in an empty fixture home; tests can override individual
// paths without ever making the developer's history a prerequisite for passing.
func TestMain(m *testing.M) {
	// Helper subprocesses must inherit the calling test's more specific fixture
	// paths, rather than replacing them with a second empty home.
	if os.Getenv("AX_APP_TEST_ISOLATED") == "1" {
		os.Exit(m.Run())
	}
	root, err := os.MkdirTemp("", "ax-app-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := func() int {
		defer os.RemoveAll(root)
		for name, value := range map[string]string{
			"AX_APP_TEST_ISOLATED": "1",
			"HOME":                 root,
			"USERPROFILE":          root,
			"XDG_CONFIG_HOME":      filepath.Join(root, "config"),
			"XDG_STATE_HOME":       filepath.Join(root, "state"),
			"XDG_DATA_HOME":        filepath.Join(root, "data"),
			"CLAUDE_CONFIG_DIR":    "",
			"CODEX_HOME":           "",
			"PI_CODING_AGENT_DIR":  "",
			"AX_CONFIG":            "",
		} {
			if err := os.Setenv(name, value); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		cfgDir := filepath.Join(root, "config", "ax")
		if err := os.MkdirAll(cfgDir, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("auto_stores = false\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}
