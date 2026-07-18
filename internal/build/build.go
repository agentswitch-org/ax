// Package build holds build-time metadata.
package build

import "strings"

// Version is the ax release or git describe string, shown in the picker navbar
// and help. It can be injected via:
// -X github.com/agentswitch-org/ax/internal/build.Version=VERSION
var Version = "0.1.0-dev"

// Display is the user-facing version string, always with a single leading "v".
// Injection paths disagree on the prefix (goreleaser strips it, git describe
// keeps it), so every renderer uses this instead of prepending its own "v".
func Display() string {
	return "v" + strings.TrimPrefix(Version, "v")
}

// Commit and Date are set via ldflags at release build time; empty in dev builds.
var Commit string
var Date string
