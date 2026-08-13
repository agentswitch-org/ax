package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
)

// The coordinate defaults must mirror the reference recipe: fenced to
// .coordinator state, no subagents, best-effort fence, capped workers/depth,
// keep-live, self-propelled, attached — and every explicit caller value wins.
func TestCoordinateOptsDefaultsAndOverrides(t *testing.T) {
	cfg := config.Config{BehaviorsDir: t.TempDir()}
	h := config.Harness{Name: "claude", Format: "claude"}

	o := coordinateOpts(cfg, h, launchOpts{task: "ship it"}, false)
	if len(o.fen.writeGlobs) != 1 || o.fen.writeGlobs[0] != "./.coordinator/**/*.md" {
		t.Errorf("writeGlobs = %v, want the .coordinator fence", o.fen.writeGlobs)
	}
	if !o.fen.noSubagents || !o.keepLive || !o.attach || !o.selfPropel {
		t.Errorf("defaults: noSubagents=%v keepLive=%v attach=%v selfPropel=%v, want all true",
			o.fen.noSubagents, o.keepLive, o.attach, o.selfPropel)
	}
	if o.fenceMode != "best-effort" || o.fen.maxWorkers != 2 || o.fen.maxDepth != 2 {
		t.Errorf("fenceMode=%q maxWorkers=%d maxDepth=%d, want best-effort/2/2", o.fenceMode, o.fen.maxWorkers, o.fen.maxDepth)
	}
	if o.name != "coordinator" || !hasLabelKey(o.labels, "role") {
		t.Errorf("name=%q labels=%v, want coordinator identity", o.name, o.labels)
	}
	if o.behavior == "" {
		t.Error("behavior not materialized")
	}

	over := coordinateOpts(cfg, h, launchOpts{
		task: "x", name: "boss", fenceMode: "strict",
		fen:      fences{writeGlobs: []string{"./docs/**"}, maxWorkers: 5, maxDepth: 1},
		labels:   []string{"role=lead"},
		behavior: "/my/behavior.md",
	}, false)
	if over.name != "boss" || over.fenceMode != "strict" || over.fen.maxWorkers != 5 || over.fen.maxDepth != 1 {
		t.Errorf("explicit values must win: %+v", over)
	}
	if over.behavior != "/my/behavior.md" || over.fen.writeGlobs[0] != "./docs/**" {
		t.Errorf("explicit behavior/fence must win: behavior=%q globs=%v", over.behavior, over.fen.writeGlobs)
	}
	if got := len(over.labels); got != 1 {
		t.Errorf("role label must not be duplicated, labels=%v", over.labels)
	}
}

// First use writes the bundled behavior into behaviors_dir (inspectable and
// editable); a user's existing copy is never overwritten.
func TestMaterializeCoordinatorBehavior(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{BehaviorsDir: dir}

	path, text := materializeCoordinatorBehavior(cfg, false)
	if text != "" || path != filepath.Join(dir, "coordinator.md") {
		t.Fatalf("materialize = (%q, %d bytes inline), want the written path", path, len(text))
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "coordinator") {
		t.Fatalf("written behavior unreadable or empty: %v", err)
	}

	// A customized copy survives.
	if err := os.WriteFile(path, []byte("MY CUSTOM BEHAVIOR"), 0o644); err != nil {
		t.Fatal(err)
	}
	path2, _ := materializeCoordinatorBehavior(cfg, false)
	data, _ = os.ReadFile(path2)
	if string(data) != "MY CUSTOM BEHAVIOR" {
		t.Error("materialize overwrote the user's customized behavior")
	}

	// No behaviors_dir at all: the text rides inline instead of failing.
	if p, txt := materializeCoordinatorBehavior(config.Config{}, false); p != "" || txt == "" {
		t.Errorf("no behaviors_dir: (path=%q, %d bytes), want inline text", p, len(txt))
	}
}

func TestSelfPropelSupported(t *testing.T) {
	for format, want := range map[string]bool{"pi": true, "codex": true, "claude": true, "opencode": false, "": false} {
		if got := selfPropelSupported(format); got != want {
			t.Errorf("selfPropelSupported(%q) = %v, want %v", format, got, want)
		}
	}
}
