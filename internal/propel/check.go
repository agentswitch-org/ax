package propel

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/shell"
)

type CheckResult struct {
	Passed bool
	Output string
}

// RunCheck bounds check runtime and captured output; failed-check evidence can
// go straight into the next targeted repair without another model reading logs.
func RunCheck(command, dir string) CheckResult {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return runCheck(ctx, command, dir)
}

func runCheck(ctx context.Context, command, dir string) CheckResult {
	if strings.TrimSpace(command) == "" {
		return CheckResult{}
	}
	base := shell.Command(command)
	c := exec.CommandContext(ctx, base.Path, base.Args[1:]...)
	c.Dir = dir
	c.WaitDelay = time.Second
	var out checkTail
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	result := CheckResult{Passed: err == nil, Output: strings.TrimSpace(string(out))}
	if ctx.Err() != nil {
		result.Output = "acceptance check interrupted: " + ctx.Err().Error()
	} else if err != nil && result.Output == "" {
		result.Output = err.Error()
	}
	return result
}

type checkTail []byte

func (b *checkTail) Write(p []byte) (int, error) {
	const limit = 2048
	n := len(p)
	if n >= limit {
		*b = append((*b)[:0], p[n-limit:]...)
	} else {
		if extra := len(*b) + n - limit; extra > 0 {
			*b = (*b)[extra:]
		}
		*b = append(*b, p...)
	}
	return n, nil
}
