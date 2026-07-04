package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const defaultPwshTimeoutSeconds = 300

// StderrTailCap bounds how many trailing bytes of stderr are surfaced in error
// diagnostics; scripts emit the operative error last, so the tail is kept.
const StderrTailCap = 1000

// Pwsh I/O contract keys name the request input fields and result envelope
// fields exchanged between the pwsh provider and the pwsh.run builtin task.
// These constants are not the sole definition of the key strings: the same
// literals also appear as json struct tags on builtin.PwshRunInput, so any
// rename must update both sides together to keep the contract in sync.
const (
	PwshInputScript         = "script"
	PwshInputArgs           = "args"
	PwshInputCwd            = "cwd"
	PwshInputTimeoutSeconds = "timeoutSeconds"

	PwshOutputResult   = "result"
	PwshOutputStdout   = "stdout"
	PwshOutputStderr   = "stderr"
	PwshOutputExitCode = "exitCode"
	PwshOutputTimedOut = "timedOut"
)

// commandRunner abstracts process execution. Production always uses the
// os/exec-backed execRunner; the seam exists only so provider-package tests can
// substitute a command double without spawning a real shell.
type commandRunner interface {
	Run(ctx context.Context, args []string, dir string) (stdout string, stderr string, exitCode int, err error)
}

// execRunner is the default commandRunner backed by os/exec.
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

// pwshRequest is the validated, typed form of a pwsh provider Request.Input.
// Decoding into it rejects malformed fields at the provider boundary instead of
// silently coercing them: a non-string script or cwd, a scalar (rather than an
// array) args value, or a timeoutSeconds that is not a positive integer are
// errors here rather than being stringified, wrapped, or dropped in favor of
// the default.
type pwshRequest struct {
	script         string
	args           []string
	cwd            string
	timeoutSeconds int
}

// decodePwshRequest validates input against the pwsh I/O contract and builds a
// typed pwshRequest, returning an error that names the first field whose value
// has the wrong shape. Fields unrelated to the provider (e.g. the pwsh.run
// retry/exit knobs) are ignored.
func decodePwshRequest(input map[string]any) (pwshRequest, error) {
	req := pwshRequest{timeoutSeconds: defaultPwshTimeoutSeconds}

	script, err := pwshStringField(input, PwshInputScript, true)
	if err != nil {
		return pwshRequest{}, err
	}
	req.script = script

	cwd, err := pwshStringField(input, PwshInputCwd, false)
	if err != nil {
		return pwshRequest{}, err
	}
	req.cwd = cwd

	args, err := pwshArgs(input[PwshInputArgs])
	if err != nil {
		return pwshRequest{}, err
	}
	req.args = args

	if v, present := input[PwshInputTimeoutSeconds]; present && v != nil {
		n, ok := intValue(v)
		if !ok || n <= 0 {
			return pwshRequest{}, fmt.Errorf("pwsh: %s must be a positive integer, got %v", PwshInputTimeoutSeconds, v)
		}
		req.timeoutSeconds = n
	}

	return req, nil
}

// pwshStringField reads a string-typed contract field. A present value of any
// other type is rejected rather than stringified; a required field that is
// absent (or an empty string) is likewise an error.
func pwshStringField(input map[string]any, key string, required bool) (string, error) {
	v, present := input[key]
	if !present || v == nil {
		if required {
			return "", fmt.Errorf("pwsh: %s is required", key)
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("pwsh: %s must be a string, got %T", key, v)
	}
	if required && s == "" {
		return "", fmt.Errorf("pwsh: %s is required", key)
	}
	return s, nil
}

func (p *PwshProvider) run(ctx context.Context, req Request) (Result, error) {
	decoded, err := decodePwshRequest(req.Input)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(decoded.timeoutSeconds)*time.Second)
	defer cancel()

	scriptFile, err := os.CreateTemp("", "taskpilot-pwsh-*.ps1")
	if err != nil {
		return nil, err
	}
	scriptPath := scriptFile.Name()
	defer os.Remove(scriptPath)
	if _, err := scriptFile.WriteString(decoded.script); err != nil {
		scriptFile.Close()
		return nil, err
	}
	if err := scriptFile.Close(); err != nil {
		return nil, err
	}

	cmdArgs := append([]string{"-NoProfile", "-NonInteractive", "-File", scriptPath}, decoded.args...)
	stdout, stderr, exitCode, err := p.runner.Run(runCtx, cmdArgs, decoded.cwd)
	if err != nil {
		return nil, err
	}
	result := map[string]any{PwshOutputStdout: stdout, PwshOutputStderr: stderr, PwshOutputExitCode: exitCode}
	// Distinguish a deadline-kill from an ordinary non-zero exit: a killed
	// process otherwise looks like a generic failure. Callers (the pwsh task)
	// use this to decide whether a timeout is retryable.
	if runCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		result[PwshOutputTimedOut] = true
	}
	if exitCode == 0 {
		parsed, err := ParsePwshStdout(stdout)
		if err != nil {
			return nil, decorateStdoutError(err, stderr)
		}
		result[PwshOutputResult] = parsed
	}
	return result, nil
}

// decorateStdoutError appends a trimmed stderr snippet to a stdout-parse error.
// A script can exit 0 yet emit a diagnostic on stderr (or nothing at all);
// surfacing stderr turns an opaque "stdout is empty" into the script's own
// message when it has one.
func decorateStdoutError(err error, stderr string) error {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return err
	}
	if len(trimmed) > StderrTailCap {
		trimmed = "..." + trimmed[len(trimmed)-StderrTailCap:]
	}
	return fmt.Errorf("%w; stderr: %s", err, trimmed)
}

// ParsePwshStdout parses the JSON value emitted by a successful pwsh script.
// It is exported so dry-run planning can produce the same output envelope as a
// real provider execution.
func ParsePwshStdout(stdout string) (any, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return nil, fmt.Errorf("pwsh stdout is empty: expected JSON")
	}
	var result any
	if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
		return nil, fmt.Errorf("parse pwsh stdout JSON: %w", err)
	}
	return result, nil
}
