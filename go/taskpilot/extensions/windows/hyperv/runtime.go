//go:build windows

package hyperv

import (
	"bytes"
	"context"
	"errors"
	"os/exec"

	"golang.org/x/sync/semaphore"
)

// gate limits Hyper-V work behind an optional semaphore.
type gate struct {
	sem *semaphore.Weighted
}

// newGate creates an optional concurrency gate.
func newGate(limit int) *gate {
	if limit <= 0 {
		return &gate{}
	}
	return &gate{sem: semaphore.NewWeighted(int64(limit))}
}

// run invokes fn under the optional concurrency gate.
func (g *gate) run(ctx context.Context, fn func(context.Context) (any, error)) (any, error) {
	if g.sem != nil {
		if err := g.sem.Acquire(ctx, 1); err != nil {
			return nil, err
		}
		defer g.sem.Release(1)
	}
	return fn(ctx)
}

// commandRunner executes PowerShell commands through a common interface.
type commandRunner interface {
	Run(ctx context.Context, args []string, dir string) (stdout string, stderr string, exitCode int, err error)
}

// execRunner runs PowerShell commands with a shared output contract.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, args []string, dir string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, "pwsh", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return stdout.String(), stderr.String(), exit.ExitCode(), nil
		}
		return "", "", 0, err
	}
	return stdout.String(), stderr.String(), 0, nil
}

// intValue coerces integer-like values from decoded option maps.
func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	}
	return 0, false
}
