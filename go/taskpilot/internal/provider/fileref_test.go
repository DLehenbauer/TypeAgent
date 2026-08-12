package provider

import (
	"strings"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

func TestBuildPromptRendersFileRefsAsPaths(t *testing.T) {
	input := map[string]any{
		copilotKeyPrompt: "Review it.",
		copilotKeyContext: map[string]any{
			"file":   "pkg/x.go",
			"source": model.FileRef("/repo/pkg/x.go", "deadbeef"),
		},
	}
	prompt, err := buildPrompt(input)
	if err != nil {
		t.Fatal(err)
	}
	// The path is surfaced for the agent to read on demand...
	if !strings.Contains(prompt, "/repo/pkg/x.go") {
		t.Fatalf("prompt missing referenced path:\n%s", prompt)
	}
	// ...and it is listed in the read-on-demand section...
	if !strings.Contains(prompt, "Read them with your tools") {
		t.Fatalf("prompt missing read-on-demand guidance:\n%s", prompt)
	}
	// ...while the identity-only fingerprint and envelope key stay out of tokens.
	if strings.Contains(prompt, "deadbeef") || strings.Contains(prompt, "$file") {
		t.Fatalf("prompt leaked fingerprint or envelope key:\n%s", prompt)
	}
}

func TestBuildPromptWithoutFileRefsHasNoFilesSection(t *testing.T) {
	input := map[string]any{
		copilotKeyPrompt:  "Summarize.",
		copilotKeyContext: map[string]any{"note": "plain data"},
	}
	prompt, err := buildPrompt(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "Read them with your tools") {
		t.Fatalf("unexpected read-on-demand section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "plain data") {
		t.Fatalf("prompt missing context data:\n%s", prompt)
	}
}

func TestReplaceFileRefsWalksNestedStructures(t *testing.T) {
	v := map[string]any{
		"list": []any{
			model.FileRef("/a", "1"),
			map[string]any{"nested": model.FileRef("/b", "2")},
		},
		"dup": model.FileRef("/a", "1"),
	}
	out, paths := replaceFileRefs(v)
	// Paths are de-duplicated in first-seen order.
	if len(paths) != 2 {
		t.Fatalf("paths = %v, want 2 unique", paths)
	}
	m := out.(map[string]any)
	list := m["list"].([]any)
	if list[0] != "/a" {
		t.Fatalf("list[0] = %v, want /a", list[0])
	}
	if list[1].(map[string]any)["nested"] != "/b" {
		t.Fatalf("nested ref not collapsed: %v", list[1])
	}
	if m["dup"] != "/a" {
		t.Fatalf("dup ref not collapsed: %v", m["dup"])
	}
}
