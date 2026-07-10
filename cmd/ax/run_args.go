package main

import (
	"fmt"
	"os"
	"strings"
)

type parsedRunArgs struct {
	id       string
	command  string
	adopt    string
	holdFail bool
}

func parseRunArgs(args []string) (parsedRunArgs, error) {
	var out parsedRunArgs
	cmdFile := ""
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		switch args[0] {
		case "--hold":
			out.holdFail, args = true, args[1:]
		case "--adopt":
			if len(args) < 2 {
				return out, fmt.Errorf("--adopt needs a harness")
			}
			out.adopt, args = args[1], args[2:]
		case "--cmd-file":
			if len(args) < 2 {
				return out, fmt.Errorf("--cmd-file needs a path")
			}
			cmdFile, args = args[1], args[2:]
		default:
			return out, fmt.Errorf("unknown run flag %s", args[0])
		}
	}
	if cmdFile != "" {
		if len(args) < 1 {
			return out, fmt.Errorf("--cmd-file needs a session id")
		}
		data, err := os.ReadFile(cmdFile)
		if err != nil {
			return out, fmt.Errorf("--cmd-file %s: %w", cmdFile, err)
		}
		_ = os.Remove(cmdFile)
		out.id, out.command = args[0], string(data)
		return out, nil
	}
	if len(args) < 2 {
		return out, fmt.Errorf("run needs a session id and command")
	}
	out.id, out.command = args[0], args[1]
	return out, nil
}
