package provider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/script"
)

// StderrTailCap bounds how many trailing bytes of stderr are surfaced in error
// diagnostics; scripts emit the operative error last, so the tail is kept.
const StderrTailCap = script.StderrTailCap

// Pwsh I/O contract keys name the request input fields and result envelope
// fields exchanged between the pwsh provider and the pwsh.run builtin task.
// These constants are not the sole definition of the key strings: the same
// literals also appear as json struct tags on builtin.PwshRunInput, so any
// rename must update both sides together to keep the contract in sync.
const (
	PwshInputScript         = script.InputScript
	PwshInputArgs           = script.InputArgs
	PwshInputCwd            = script.InputCwd
	PwshInputTimeoutSeconds = script.InputTimeoutSeconds

	PwshOutputResult   = script.OutputResult
	PwshOutputStdout   = script.OutputStdout
	PwshOutputStderr   = script.OutputStderr
	PwshOutputExitCode = script.OutputExitCode
	PwshOutputTimedOut = script.OutputTimedOut
)

// commandRunner abstracts process execution. Production always uses the
// os/exec-backed execRunner; the seam exists only so provider-package tests can
// substitute a command double without spawning a real shell.
type commandRunner interface {
	Run(ctx context.Context, args []string, dir string) (stdout string, stderr string, exitCode int, err error)
}

// execRunner is the default commandRunner backed by os/exec.
type execRunner struct{}

// Run starts pwsh with the supplied arguments and returns its captured output.
// Non-exit execution errors discard any captured output.
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
		// Converts a non-zero PowerShell exit into a regular result instead of a
		// host-level execution failure so callers can decide how to handle it.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.String(), stderr.String(), ee.ExitCode(), nil
		}
		return "", "", 0, err
	}
	return stdout.String(), stderr.String(), 0, nil
}

// PwshProvider executes PowerShell scripts.
type PwshProvider struct {
	runner   commandRunner
	throttle *throttle
}

// NewPwshProvider returns a PwshProvider backed by the real os/exec runner.
// limit bounds concurrent script executions; a non-positive limit runs scripts
// unbounded.
func NewPwshProvider(limit int) *PwshProvider {
	return &PwshProvider{runner: execRunner{}, throttle: newThrottle(limit)}
}

// Name returns NamePwsh, the registry name under which this provider is
// registered and looked up.
func (p *PwshProvider) Name() Name { return NamePwsh }

// Submit runs the script once. Non-zero exit codes are returned as results, not
// errors, so the pwsh task can classify them as retry/fail/accept. Successful
// runs must emit JSON on stdout; it is parsed into the result field while stdout
// remains available for diagnostics.
func (p *PwshProvider) Submit(ctx context.Context, req Request) Future {
	return p.throttle.dispatch(ctx, func(ctx context.Context) (Result, error) {
		return p.run(ctx, req)
	})
}

// run executes one PowerShell script request and maps its exit status into a
// provider result.
func (p *PwshProvider) run(ctx context.Context, req Request) (Result, error) {
	decoded, err := script.Decode(req.Input, model.FileRefPath)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(decoded.TimeoutSeconds)*time.Second)
	defer cancel()

	// Write the script to a temp file because pwsh -File accepts a path.
	scriptFile, err := os.CreateTemp("", "taskpilot-pwsh-*.ps1")
	if err != nil {
		return nil, err
	}
	scriptPath := scriptFile.Name()
	defer os.Remove(scriptPath)
	if _, err := scriptFile.WriteString(decoded.Script); err != nil {
		scriptFile.Close()
		return nil, err
	}
	if err := scriptFile.Close(); err != nil {
		return nil, err
	}

	cmdArgs := append([]string{"-NoProfile", "-NonInteractive", "-File", scriptPath}, decoded.Args...)
	stdout, stderr, exitCode, err := p.runner.Run(runCtx, cmdArgs, decoded.Cwd)
	if err != nil {
		return nil, err
	}
	timedOut := runCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil
	result, err := script.NewResult(stdout, stderr, exitCode, timedOut)
	if err != nil {
		return nil, err
	}
	return result.Map(), nil
}
