// Package script defines the typed contract shared by host and target script
// execution.
package script

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	DefaultTimeoutSeconds = 300
	StderrTailCap         = 1000

	InputScript         = "script"
	InputArgs           = "args"
	InputCwd            = "cwd"
	InputTimeoutSeconds = "timeoutSeconds"

	OutputResult   = "result"
	OutputStdout   = "stdout"
	OutputStderr   = "stderr"
	OutputExitCode = "exitCode"
	OutputTimedOut = "timedOut"
)

// Request is a validated script invocation.
type Request struct {
	Script         string
	Args           []string
	Cwd            string
	TimeoutSeconds int
}

// Result is a completed script invocation.
type Result struct {
	Value    any
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
}

// Decode validates the script fields in a task input. Unrelated task-level
// fields such as retry, cache, checkpoint, and runsOn are ignored.
func Decode(input map[string]any, render func(any) (string, bool)) (Request, error) {
	req := Request{TimeoutSeconds: DefaultTimeoutSeconds}

	script, err := stringField(input, InputScript, true)
	if err != nil {
		return Request{}, err
	}
	req.Script = script

	cwd, err := stringField(input, InputCwd, false)
	if err != nil {
		return Request{}, err
	}
	req.Cwd = cwd

	args, err := argsField(input[InputArgs], render)
	if err != nil {
		return Request{}, err
	}
	req.Args = args

	if value, present := input[InputTimeoutSeconds]; present && value != nil {
		n, ok := intValue(value)
		if !ok || n <= 0 {
			return Request{}, fmt.Errorf("pwsh: %s must be a positive integer, got %v", InputTimeoutSeconds, value)
		}
		req.TimeoutSeconds = n
	}
	return req, nil
}

func stringField(input map[string]any, key string, required bool) (string, error) {
	value, present := input[key]
	if !present || value == nil {
		if required {
			return "", fmt.Errorf("pwsh: %s is required", key)
		}
		return "", nil
	}
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("pwsh: %s must be a string, got %T", key, value)
	}
	if required && s == "" {
		return "", fmt.Errorf("pwsh: %s is required", key)
	}
	return s, nil
}

func argsField(value any, render func(any) (string, bool)) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("pwsh: %s must be an array, got %T", InputArgs, value)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if render != nil {
			if rendered, ok := render(item); ok {
				out = append(out, rendered)
				continue
			}
		}
		switch v := item.(type) {
		case string:
			out = append(out, v)
		default:
			out = append(out, fmt.Sprint(v))
		}
	}
	return out, nil
}

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

// ParseStdout parses the JSON value emitted by a successful script.
func ParseStdout(stdout string) (any, error) {
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

// NewResult builds a Result and parses stdout when exitCode is zero.
func NewResult(stdout, stderr string, exitCode int, timedOut bool) (Result, error) {
	result := Result{Stdout: stdout, Stderr: stderr, ExitCode: exitCode, TimedOut: timedOut}
	if exitCode == 0 {
		value, err := ParseStdout(stdout)
		if err != nil {
			return Result{}, DecorateStdoutError(err, stderr)
		}
		result.Value = value
	}
	return result, nil
}

// Map returns the stable task output envelope.
func (r Result) Map() map[string]any {
	out := map[string]any{
		OutputStdout:   r.Stdout,
		OutputStderr:   r.Stderr,
		OutputExitCode: r.ExitCode,
	}
	if r.TimedOut {
		out[OutputTimedOut] = true
	}
	if r.ExitCode == 0 {
		out[OutputResult] = r.Value
	}
	return out
}

// DecorateStdoutError appends the useful tail of stderr to a parse error.
func DecorateStdoutError(err error, stderr string) error {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return err
	}
	if len(trimmed) > StderrTailCap {
		trimmed = "..." + trimmed[len(trimmed)-StderrTailCap:]
	}
	return fmt.Errorf("%w; stderr: %s", err, trimmed)
}
