// Package build holds build-time metadata.
package build

// Version is the ax release or git describe string, shown in the picker navbar
// and help. It can be injected via:
// -X github.com/agentswitch-org/ax/internal/build.Version=VERSION
var Version = "0.1.0-dev"

// Commit and Date are set via ldflags at release build time; empty in dev builds.
var Commit string
var Date string
