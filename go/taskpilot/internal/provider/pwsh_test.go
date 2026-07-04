package provider

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

type captureRunner struct {
	args []string
	dir  string

	stdout   string
	stderr   string
	exitCode int
	err      error
}

func (c *captureRunner) Run(ctx context.Context, args []string, dir string) (string, string, int, error) {
	c.args = args
	c.dir = dir
	return c.stdout, c.stderr, c.exitCode, c.err
}

// newTestPwshProvider wires a command double into a PwshProvider. It keeps the
// runner substitution in test-only code so the production constructor never
// exposes an injection seam.
func newTestPwshProvider(runner commandRunner) *PwshProvider {
	return &PwshProvider{runner: runner, throttle: newThrottle(0)}
}

func TestPwshProviderBindsArgsWithoutSpawning(t *testing.T) {
	runner := &captureRunner{stdout: `{"ok":true}`}
	p := newTestPwshProvider(runner)

	out, err := p.Submit(context.Background(), Request{Input: map[string]any{
		PwshInputScript: `Write-Output "hi"`,
		PwshInputArgs:   []any{`D:\tvm`, 3},
		PwshInputCwd:    `C:\work`,
	}}).Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if runner.dir != `C:\work` {
		t.Fatalf("dir = %q, want C:\\work", runner.dir)
	}
	wantPrefix := []string{"-NoProfile", "-NonInteractive", "-File"}
	for i, w := range wantPrefix {
		if runner.args[i] != w {
			t.Fatalf("args[%d] = %q, want %q", i, runner.args[i], w)
		}
	}
	if !strings.HasSuffix(runner.args[3], ".ps1") {
		t.Fatalf("script path = %q, want *.ps1", runner.args[3])
	}
	if got := runner.args[4:]; got[0] != `D:\tvm` || got[1] != "3" {
		t.Fatalf("bound args = %v, want [D:\\tvm 3]", got)
	}

	result := out.(map[string]any)
	if result[PwshOutputStdout] != `{"ok":true}` || result[PwshOutputExitCode] != 0 {
		t.Fatalf("result = %#v", result)
	}
	parsed := result[PwshOutputResult].(map[string]any)
	if parsed["ok"] != true {
		t.Fatalf("result = %#v", result)
	}
}

func TestPwshProviderErrorsOnInvalidSuccessJSON(t *testing.T) {
	runner := &captureRunner{stdout: "not json"}
	p := newTestPwshProvider(runner)

	_, err := p.Submit(context.Background(), Request{Input: map[string]any{
		PwshInputScript: `Write-Output "hi"`,
	}}).Await(context.Background())
	if err == nil {
		t.Fatal("expected invalid JSON error")
	}
}

func TestPwshProviderEmptyStdoutErrorIncludesStderr(t *testing.T) {
	runner := &captureRunner{stdout: "", stderr: "preflight: working tree is not clean\n"}
	p := newTestPwshProvider(runner)

	_, err := p.Submit(context.Background(), Request{Input: map[string]any{
		PwshInputScript: `exit 0`,
	}}).Await(context.Background())
	if err == nil {
		t.Fatal("expected stdout-empty error")
	}
	if !strings.Contains(err.Error(), "stdout is empty") || !strings.Contains(err.Error(), "working tree is not clean") {
		t.Fatalf("error = %q, want stdout-empty plus stderr", err)
	}
}

func TestPwshProviderRejectsMalformedInput(t *testing.T) {
	cases := map[string]map[string]any{
		"missing script":       {},
		"non-string script":    {PwshInputScript: 42},
		"non-string cwd":       {PwshInputScript: `exit 0`, PwshInputCwd: 42},
		"scalar args":          {PwshInputScript: `exit 0`, PwshInputArgs: "one"},
		"fractional timeout":   {PwshInputScript: `exit 0`, PwshInputTimeoutSeconds: 1.5},
		"non-positive timeout": {PwshInputScript: `exit 0`, PwshInputTimeoutSeconds: 0},
		"non-numeric timeout":  {PwshInputScript: `exit 0`, PwshInputTimeoutSeconds: "30"},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &captureRunner{stdout: `{"ok":true}`}
			p := newTestPwshProvider(runner)
			_, err := p.Submit(context.Background(), Request{Input: input}).Await(context.Background())
			if err == nil {
				t.Fatal("expected error for malformed input, got nil")
			}
			if runner.args != nil {
				t.Fatalf("runner should not have been invoked, got args %v", runner.args)
			}
		})
	}
}

func TestPwshProviderReal(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not available")
	}
	p := NewPwshProvider(0)
	out, err := p.Submit(context.Background(), Request{Input: map[string]any{
		PwshInputScript: `[ordered]@{ repo = $args[0]; depth = [int]$args[1] } | ConvertTo-Json -Compress`,
		PwshInputArgs:   []any{`D:\tvm`, 3},
	}}).Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := out.(map[string]any)
	if result[PwshOutputExitCode] != 0 {
		t.Fatalf("exitCode = %v, stderr = %q", result[PwshOutputExitCode], result[PwshOutputStderr])
	}
	parsed := result[PwshOutputResult].(map[string]any)
	if parsed["repo"] != `D:\tvm` || parsed["depth"] != float64(3) {
		t.Fatalf("parsed result = %#v", parsed)
	}
}
