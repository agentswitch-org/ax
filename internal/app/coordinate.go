package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentswitch-org/ax/behaviors"
	"github.com/agentswitch-org/ax/internal/config"
)

// Coordinate is the one-command coordinator bootstrap (`ax coordinate "GOAL"`):
// the bundled behavior plus the reference recipe's guardrails; any launch flag
// passes through and wins over the defaults (see coordinateUsage).
func (a App) Coordinate(args []string) {
	cfg, _ := config.Load()
	harness := ""
	small := false
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--harness":
			harness = nextVal("--harness", args, &i)
		case "--small":
			small = true
		case "--help", "-h":
			coordinateUsage(os.Stdout)
			return
		default:
			rest = append(rest, args[i])
		}
	}

	o, err := parseLaunch(rest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(2)
	}
	if strings.TrimSpace(o.task) == "" {
		fmt.Fprintln(os.Stderr, "ax: coordinate needs a goal")
		coordinateUsage(os.Stderr)
		os.Exit(2)
	}

	h, ok := coordinateHarness(cfg, harness)
	if !ok {
		fmt.Fprintf(os.Stderr, "ax: no harness to coordinate with; pass --harness or set default_harness in config (configured: %s)\n", strings.Join(harnessNamesList(cfg), ", "))
		os.Exit(2)
	}

	a.runLaunch(h.Name, coordinateOpts(cfg, h, o, small), launchCtx{})
}

// coordinateOpts overlays the coordinator defaults onto a parsed launch;
// explicit caller values win.
func coordinateOpts(cfg config.Config, h config.Harness, o launchOpts, small bool) launchOpts {
	if o.behavior == "" && o.behaviorText == "" {
		path, text := materializeCoordinatorBehavior(cfg, small)
		o.behavior, o.behaviorText = path, text
	}
	if len(o.fen.writeGlobs) == 0 && !o.fen.noWrite {
		o.fen.writeGlobs = []string{"./.coordinator/**/*.md"}
	}
	o.fen.noSubagents = true
	if o.fenceMode == "" {
		o.fenceMode = "best-effort" // fence where enforceable, warn-and-launch where not
	}
	if o.fen.maxWorkers == 0 {
		o.fen.maxWorkers = 2
	}
	if o.fen.maxDepth == 0 {
		o.fen.maxDepth = 2
	}
	o.keepLive = true
	o.attach = true
	if o.name == "" {
		o.name = "coordinator"
	}
	if !hasLabelKey(o.labels, "role") {
		o.labels = append(o.labels, "role=coordinator")
	}
	if selfPropelSupported(h.Format) {
		o.selfPropel = true
	}
	return o
}

// coordinateHarness picks --harness, else default_harness, else claude, else the first configured.
func coordinateHarness(cfg config.Config, name string) (config.Harness, bool) {
	pick := func(n string) (config.Harness, bool) {
		for _, h := range cfg.Harnesses {
			if h.Name == n {
				return h, true
			}
		}
		return config.Harness{}, false
	}
	if name != "" {
		h, ok := pick(name)
		if !ok {
			fmt.Fprintf(os.Stderr, "ax: unknown harness %q (configured: %s)\n", name, strings.Join(harnessNamesList(cfg), ", "))
			os.Exit(2)
		}
		return h, true
	}
	if cfg.DefaultHarness != "" {
		if h, ok := pick(cfg.DefaultHarness); ok {
			return h, true
		}
	}
	if h, ok := pick("claude"); ok {
		return h, true
	}
	if len(cfg.Harnesses) > 0 {
		return cfg.Harnesses[0], true
	}
	return config.Harness{}, false
}

// materializeCoordinatorBehavior prefers the user's copy in behaviors_dir,
// writes the bundled text there on first use, and falls back to inline text
// when the directory is unwritable.
func materializeCoordinatorBehavior(cfg config.Config, small bool) (path, text string) {
	name := behaviors.CoordinatorFile(small)
	dir := config.ExpandHome(cfg.BehaviorsDir)
	if dir == "" {
		return "", behaviors.Coordinator(small)
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err == nil {
		return p, ""
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		if err := os.WriteFile(p, []byte(behaviors.Coordinator(small)), 0o644); err == nil {
			fmt.Fprintf(os.Stderr, "ax: wrote the bundled coordinator behavior to %s (edit it to customize)\n", p)
			return p, ""
		}
	}
	return "", behaviors.Coordinator(small)
}

// selfPropelSupported: pi/codex pump on transcript turn end, claude on its Stop-hook marker.
func selfPropelSupported(format string) bool {
	switch format {
	case "pi", "codex", "claude":
		return true
	}
	return false
}

func hasLabelKey(labels []string, key string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, key+"=") {
			return true
		}
	}
	return false
}

func harnessNamesList(cfg config.Config) []string {
	var names []string
	for _, h := range cfg.Harnesses {
		names = append(names, h.Name)
	}
	return names
}

func coordinateUsage(w *os.File) {
	fmt.Fprint(w, `usage: ax coordinate "GOAL" [--harness H] [--small] [launch flags]

Launch a self-propelled coordinator for the project in the current directory
(or --dir D). It triages the goal into .coordinator/backlog.md, delegates work
to tracked workers, verifies results, and keeps going until the project is
done or it genuinely needs you.

  --harness H   harness to run the coordinator on (default: default_harness,
                else claude)
  --small       use the trimmed behavior written for a small local model
  launch flags  any ax launch flag passes through and overrides the defaults
                (--model, --dir, --run, --max-workers, --propel-until, ...)

Defaults: bundled coordinator behavior (materialized into behaviors_dir on
first use), --write './.coordinator/**/*.md' --no-subagents --fence best-effort
--max-workers 2 --max-depth 2 --keep-live --self-propel --attach.
`)
}
