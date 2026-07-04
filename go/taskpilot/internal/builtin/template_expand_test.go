package builtin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

func TestTemplateExpandInlineTemplate(t *testing.T) {
	out, err := expandTemplate(nil, map[string]any{
		"template": "hello {{name}}",
		"vars":     map[string]any{"name": "world"},
	}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello world" {
		t.Fatalf("expand = %q, want %q", out, "hello world")
	}
}

func TestTemplateExpandFromPath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tmpl.md")
	if err := os.WriteFile(file, []byte("# {{title}}\nBody"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := expandTemplate(nil, map[string]any{
		"templatePath": file,
		"vars":         map[string]any{"title": "Report"},
	}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "# Report\nBody" {
		t.Fatalf("expand from path = %q, want %q", out, "# Report\nBody")
	}
}

func TestTemplateExpandPathNoVarsIsRawRead(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rubric.md")
	body := "Rule of three.\nNo placeholders here."
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := expandTemplate(nil, map[string]any{"templatePath": file}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	if out != body {
		t.Fatalf("expand path w/o vars = %q, want %q", out, body)
	}
}

func TestTemplateExpandRejectsBothSources(t *testing.T) {
	_, err := expandTemplate(nil, map[string]any{
		"template":     "inline",
		"templatePath": "somewhere.md",
	}, Context{})
	if err == nil {
		t.Fatal("expected error when both template and templatePath are set")
	}
}

func TestTemplateExpandRendersFileRefAsPath(t *testing.T) {
	const refPath = "internal/builtin/template_expand.go"
	out, err := expandTemplate(nil, map[string]any{
		"template": "Review {{source}} against the rubric.",
		"vars": map[string]any{
			"source": model.FileRef(refPath, "abc123"),
		},
	}, Context{})
	if err != nil {
		t.Fatal(err)
	}
	want := "Review " + refPath + " against the rubric."
	if out != want {
		t.Fatalf("fileref expand = %q, want %q", out, want)
	}
}

func TestTemplatePathDigest(t *testing.T) {
	// No templatePath -> stable empty digest (inline template has no external state).
	if d, err := templatePathDigest(map[string]any{"template": "x"}); err != nil || d != "" {
		t.Fatalf("inline digest = %q, err=%v; want empty", d, err)
	}
	file := filepath.Join(t.TempDir(), "t.md")
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	d1, err := templatePathDigest(map[string]any{"templatePath": file})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	d2, err := templatePathDigest(map[string]any{"templatePath": file})
	if err != nil {
		t.Fatal(err)
	}
	if d1 == "" || d1 == d2 {
		t.Fatalf("templatePath digest did not track content: %q vs %q", d1, d2)
	}
}
