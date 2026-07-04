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
		// A resolved fileref var collapses to its on-disk path -- matching how
		// the Copilot and pwsh consumers render references -- so the var expands
		// to the path the prompt tells the agent to read, not the raw envelope.
		if p, ok := model.FileRefPath(v); ok {
			return p
		}
		return fmt.Sprint(v)
	}), nil
}

// templateText resolves the template source: the inline Template, or the
// contents of TemplatePath when set. TemplatePath is the sanctioned way to
// materialize a prompt or rubric from disk -- reading the file and expanding it
// in one content-addressed node -- so prompt text never has to travel through a
// separate whole-file read. Supplying both is a configuration error.
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

// templatePathDigest is the external-state digester for template.expand. When
// the template is read from templatePath, the node's output depends on that
// file's content, so its identity folds in a content hash and it re-runs when
// the file changes. An inline template has no external state and yields a
// stable empty digest.
func templatePathDigest(input map[string]any) (string, error) {
	path := asString(input["templatePath"])
	if path == "" {
		return "", nil
	}
	return hashFileContent(path)
}
