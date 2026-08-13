// Package behaviors embeds the bundled coordinator behaviors into the binary,
// so `ax coordinate` works on a fresh machine with zero setup. The .md files
// here stay the canonical sources.
package behaviors

import _ "embed"

//go:embed coordinator.md
var coordinator string

//go:embed coordinator-small.md
var coordinatorSmall string

// Coordinator returns the bundled coordinator behavior text. small selects the
// trimmed variant written for a weak local model (one worker at a time).
func Coordinator(small bool) string {
	if small {
		return coordinatorSmall
	}
	return coordinator
}

// CoordinatorFile is the behavior's canonical filename inside behaviors_dir;
// small selects the trimmed variant's name.
func CoordinatorFile(small bool) string {
	if small {
		return "coordinator-small.md"
	}
	return "coordinator.md"
}
