package builtin

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// TemplateExpandInput carries the inputs for the template.expand task.
// TemplatePath reads template text from disk and must not be combined with
// Template; Vars supplies placeholder values.
type TemplateExpandInput struct {
	Template     string         `json:"template,omitempty"`
	TemplatePath string         `json:"templatePath,omitempty"`
	Vars         map[string]any `json:"vars,omitempty"`
}

var templateExpandSpec = model.TaskSpec{
	Name:        "template.expand",
	Version:     "1",
	InputSchema: structToSchema(reflect.TypeOf(TemplateExpandInput{})),
}

var tmplRE = regexp.MustCompile(`\{\{\s*(?:[A-Za-z0-9_.-]+)\s*\}\}`)

// expandTemplate replaces placeholders with Vars values.
// Missing values expand to "", and file references expand to their paths.
func expandTemplate(_ context.Context, input map[string]any, _ Context) (any, error) {
	in, err := decodeInput[TemplateExpandInput](input)
	if err != nil {
		return nil, err
	}
	text, err := templateText(in)
	if err != nil {
		return nil, err
	}
	vars := in.Vars
	return tmplRE.ReplaceAllStringFunc(text, func(match string) string {
		key := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(match, "{{"), "}}"))
		v, ok := lookup(vars, strings.Split(key, "."))
		if !ok || v == nil {
			return ""
		}
		if path, ok := model.FileRefPath(v); ok {
			return path
		}
		return fmt.Sprint(v)
	}), nil
}

// templateText returns inline template text, or reads TemplatePath when set.
// Supplying both Template and TemplatePath is invalid.
func templateText(in TemplateExpandInput) (string, error) {
	if in.TemplatePath == "" {
		return in.Template, nil
	}
	if in.Template != "" {
		return "", fmt.Errorf("template.expand: set only one of template or templatePath")
	}
	b, err := os.ReadFile(in.TemplatePath)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// templatePathDigest hashes TemplatePath content for cache identity.
// Inline templates have no external-state digest.
func templatePathDigest(input map[string]any) (string, error) {
	path := asString(input["templatePath"])
	if path == "" {
		return "", nil
	}
	return hashFileContent(path)
}
