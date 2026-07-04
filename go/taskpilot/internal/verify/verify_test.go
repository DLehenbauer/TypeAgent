package verify

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/builtin"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// minimalValidDocument returns a baseline valid document with a single "main"
// task whose graph has no nodes. Tests mutate the returned value to exercise
// specific verification paths.
func minimalValidDocument() *model.Document {
	return &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "main",
		Tasks: map[string]model.TaskDef{
			"main": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph:        &model.Graph{Nodes: map[string]model.Node{}, Output: map[string]any{}},
			},
		},
	}
}

func TestDocumentDetectsCycle(t *testing.T) {
	doc := minimalValidDocument()
	doc.Tasks["main"].Graph.Nodes["a"] = model.Node{Task: "template.expand", DependsOn: []string{"b"}}
	doc.Tasks["main"].Graph.Nodes["b"] = model.Node{Task: "template.expand", DependsOn: []string{"a"}}
	res, err := Document(doc, builtin.SchemaRegistry())
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if res != nil {
		t.Fatalf("expected nil VerifiedDocument on failure, got %v", res)
	}
}

func TestDocumentRequiresEngineConstraint(t *testing.T) {
	engineVersion := model.EngineVersion()
	major, err := strconv.Atoi(strings.SplitN(engineVersion, ".", 2)[0])
	if err != nil {
		t.Fatalf("cannot derive major version from %q: %v", engineVersion, err)
	}
	nextMajor := fmt.Sprintf("%d.0.0", major+1)

	tests := []struct {
		name       string
		requires   string
		wantErrSub string
	}{
		{name: "compatible exact", requires: engineVersion},
		{name: "compatible range", requires: ">=" + engineVersion + " <" + nextMajor},
		{name: "incompatible", requires: ">=" + nextMajor, wantErrSub: "requiresEngine:"},
		{name: "invalid semver", requires: "banana", wantErrSub: "invalid semver"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := minimalValidDocument()
			doc.RequiresEngine = tt.requires

			res, err := Document(doc, builtin.SchemaRegistry())
			if tt.wantErrSub == "" {
				if err != nil {
					t.Fatalf("expected valid document, got %v", err)
				}
				if res == nil {
					t.Fatal("expected VerifiedDocument on success, got nil")
				}
				return
			}

			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("expected *ValidationError, got %v", err)
			}
			found := false
			for _, e := range verr.Errors {
				if strings.Contains(e, tt.wantErrSub) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected error containing %q, got %v", tt.wantErrSub, verr.Errors)
			}
		})
	}
}
