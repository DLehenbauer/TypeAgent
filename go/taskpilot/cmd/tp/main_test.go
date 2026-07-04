package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
)

func TestRunStreamsProgressToStderrByDefault(t *testing.T) {
	t.Setenv("TASKPILOT_STATE_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer

	if err := run([]string{"run", helloExamplePath(), "--name", "world"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "run started") {
		t.Fatalf("stderr missing live progress: %q", stderr.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("stdout is not final JSON output: %q: %v", stdout.String(), err)
	}
	if output["message"] != "Hello, world!" {
		t.Fatalf("message = %v", output["message"])
	}
}

func TestRunQuietSuppressesProgress(t *testing.T) {
	t.Setenv("TASKPILOT_STATE_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer

	if err := run([]string{"run", helloExamplePath(), "--name", "world", "--quiet"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunPrintsSummaryAndWritesOutputFile(t *testing.T) {
	state := t.TempDir()
	t.Setenv("TASKPILOT_STATE_DIR", state)
	var stdout, stderr bytes.Buffer

	if err := run([]string{"run", helloExamplePath(), "--name", "world"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	es := stderr.String()
	for _, want := range []string{"succeeded in", "hit(s)", "miss(es)", "log:", "output:", "cache:"} {
		if !strings.Contains(es, want) {
			t.Fatalf("stderr missing %q: %q", want, es)
		}
	}

	runID := onlyRunID(t, state)
	outPath := filepath.Join(state, "logs", runID+".output.json")
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("output file not written: %v", err)
	}
	if strings.TrimSpace(string(data)) != strings.TrimSpace(stdout.String()) {
		t.Fatalf("output file (%q) != stdout (%q)", data, stdout.String())
	}
}

func TestPathsCommandReportsResolvedLocations(t *testing.T) {
	state := t.TempDir()
	t.Setenv("TASKPILOT_STATE_DIR", state)
	var stdout, stderr bytes.Buffer
	if err := run([]string{"paths"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	got := stdout.String()
	for _, want := range []string{
		"state\t" + state,
		"logs\t" + filepath.Join(state, "logs"),
		"cache\t" + filepath.Join(state, "cache"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("paths output missing %q: %q", want, got)
		}
	}
}

func TestLogCommandPrintsCachedOutputPreview(t *testing.T) {
	state := t.TempDir()
	t.Setenv("TASKPILOT_STATE_DIR", state)
	var stdout, stderr bytes.Buffer
	if err := run([]string{"run", helloExamplePath(), "--name", "world", "--quiet"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	runID := onlyRunID(t, state)
	stdout.Reset()
	if err := run([]string{"log", runID}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	got := stdout.String()
	if !strings.Contains(got, "node.completed") {
		t.Fatalf("log output missing completed node: %q", got)
	}
	if !strings.Contains(got, `output: "Hello, world!"`) {
		t.Fatalf("log output missing cached output preview: %q", got)
	}
}

func TestRunFailurePrintsRootCauseAndSharedPaths(t *testing.T) {
	state := t.TempDir()
	t.Setenv("TASKPILOT_STATE_DIR", state)

	missing := filepath.Join(t.TempDir(), "does-not-exist.txt")
	wf := filepath.Join(t.TempDir(), "fail.yaml")
	// Single-quoted YAML scalar keeps the Windows path's backslashes literal.
	doc := "kind: taskpilot\n" +
		"version: 1\n" +
		"entry: readit\n" +
		"tasks:\n" +
		"  readit:\n" +
		"    inputSchema:\n" +
		"      type: object\n" +
		"    outputSchema:\n" +
		"      type: object\n" +
		"      required: [content]\n" +
		"      properties:\n" +
		"        content:\n" +
		"          type: string\n" +
		"    graph:\n" +
		"      nodes:\n" +
		"        content:\n" +
		"          task: file.readJson\n" +
		"          inputs:\n" +
		"            path: '" + missing + "'\n" +
		"      output:\n" +
		"        content:\n" +
		"          $from: node\n" +
		"          node: content\n"
	if err := os.WriteFile(wf, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"run", wf}, &stdout, &stderr); err == nil {
		t.Fatalf("expected run to fail; stderr=%q", stderr.String())
	}
	es := stderr.String()
	// Prominent root-cause block: node, task, and stage where the error began.
	for _, want := range []string{
		"FAILED in",
		`root cause: node "content" (file.readJson) failed at stage execution`,
		"error:",
		"debug:    tp log run-",
	} {
		if !strings.Contains(es, want) {
			t.Fatalf("failure summary missing %q: %q", want, es)
		}
	}
	// The artifact paths are shared with the success path (workflow, log, cache),
	// but the output file is never written on failure so the footer must not list
	// it. Scope the check to the footer: the up-front header still prints the
	// planned output path before the run starts.
	footer := es[strings.Index(es, "FAILED in"):]
	for _, want := range []string{"workflow:", "log:", "cache:"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("failure summary missing shared path %q: %q", want, footer)
		}
	}
	if strings.Contains(footer, "output:") {
		t.Fatalf("failure summary should not list output file: %q", footer)
	}
}

func helloExamplePath() string {
	return filepath.Join("..", "..", "examples", "hello.yaml")
}

func onlyRunID(t *testing.T, state string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	var runID string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		if runID != "" {
			t.Fatalf("multiple log files found in %s", filepath.Join(state, "logs"))
		}
		runID = strings.TrimSuffix(entry.Name(), ".jsonl")
	}
	if runID == "" {
		t.Fatal("no run log found")
	}
	return runID
}

func TestParseRunArgsProviderParallelFlags(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		opts, err := parseRunArgs([]string{"file.yaml"})
		if err != nil {
			t.Fatal(err)
		}
		if opts.maxParallel != defaultMaxParallel {
			t.Fatalf("maxParallel = %d, want %d", opts.maxParallel, defaultMaxParallel)
		}
		want := provider.DefaultLimits()
		if !maps.Equal(opts.limits, want) {
			t.Fatalf("limits = %v, want %v", opts.limits, want)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		opts, err := parseRunArgs([]string{"file.yaml", "--copilot-parallel", "3", "--pwsh-parallel", "16"})
		if err != nil {
			t.Fatal(err)
		}
		if opts.limits[provider.NameCopilot] != 3 {
			t.Fatalf("copilot limit = %d, want 3", opts.limits[provider.NameCopilot])
		}
		if opts.limits[provider.NamePwsh] != 16 {
			t.Fatalf("pwsh limit = %d, want 16", opts.limits[provider.NamePwsh])
		}
	})

	t.Run("invalid", func(t *testing.T) {
		if _, err := parseRunArgs([]string{"file.yaml", "--copilot-parallel", "0"}); err == nil {
			t.Fatal("expected error for non-positive --copilot-parallel")
		}
		if _, err := parseRunArgs([]string{"file.yaml", "--pwsh-parallel", "x"}); err == nil {
			t.Fatal("expected error for non-numeric --pwsh-parallel")
		}
	})
}

func TestRunAppliesInputSchemaDefaultsWhenFlagOmitted(t *testing.T) {
	t.Setenv("TASKPILOT_STATE_DIR", t.TempDir())
	doc := `kind: taskpilot
version: 1
entry: hello
tasks:
  hello:
    inputSchema:
      type: object
      required: [name]
      properties:
        name:
          type: string
          default: world
    outputSchema:
      type: object
      required: [message]
      properties:
        message:
          type: string
    graph:
      nodes:
        render:
          task: template.expand
          inputs:
            template: "Hello, {{ name }}!"
            vars:
              name:
                $from: input
                name: name
          outputSchema:
            type: string
      output:
        message:
          $from: node
          node: render
`
	path := filepath.Join(t.TempDir(), "defaults.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"run", path, "--quiet"}, &stdout, &stderr); err != nil {
		t.Fatalf("run with omitted defaulted flag failed: %v (stderr=%q)", err, stderr.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("stdout is not JSON: %q: %v", stdout.String(), err)
	}
	if output["message"] != "Hello, world!" {
		t.Fatalf("message = %v, want default applied", output["message"])
	}
}
