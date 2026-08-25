package verify

import (
	"errors"
	"fmt"
	"reflect"
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

func TestDocumentSkipsNodeRefsInsideLiteral(t *testing.T) {
	doc := minimalValidDocument()
	doc.Tasks["main"].Graph.Nodes = map[string]model.Node{
		"producer": {
			Task: "template.expand",
		},
		"literalOnly": {
			Task: "template.expand",
			Inputs: map[string]any{
				"data": map[string]any{
					"$literal": map[string]any{
						"direct": map[string]any{"$from": "node", "node": "missing"},
						"nested": []any{
							map[string]any{"$from": "node", "node": "producer"},
						},
					},
				},
			},
		},
		"ordinary": {
			Task: "template.expand",
			Inputs: map[string]any{
				"nested": []any{
					map[string]any{"$from": "node", "node": "producer"},
				},
			},
		},
	}

	verified, err := Document(doc, builtin.SchemaRegistry())
	if err != nil {
		t.Fatalf("expected literal-shaped reference data to verify, got %v", err)
	}
	wantEdges := []Edge{{From: "producer", To: "ordinary"}}
	if got := verified.Graphs["main"].Edges; !reflect.DeepEqual(got, wantEdges) {
		t.Fatalf("edges = %v, want %v", got, wantEdges)
	}
}

// TestDocumentSkipsNodeRefsInsideReferenceObject pins the walker to resolver
// semantics: resolveTemplate returns as soon as it sees a reference object and
// reads only that object's own scalar fields, so a reference nested beneath one
// is inert data. Descending into it would invent a dependency edge and reject a
// document that resolves cleanly at runtime.
func TestDocumentSkipsNodeRefsInsideReferenceObject(t *testing.T) {
	doc := minimalValidDocument()
	doc.Tasks["main"].Graph.Nodes = map[string]model.Node{
		"producer": {
			Task: "template.expand",
		},
		"consumer": {
			Task: "template.expand",
			Inputs: map[string]any{
				"data": map[string]any{
					"$from":   "node",
					"node":    "producer",
					"ignored": map[string]any{"$from": "node", "node": "missing"},
				},
			},
		},
	}

	verified, err := Document(doc, builtin.SchemaRegistry())
	if err != nil {
		t.Fatalf("expected inert nested reference to verify, got %v", err)
	}
	wantEdges := []Edge{{From: "producer", To: "consumer"}}
	if got := verified.Graphs["main"].Edges; !reflect.DeepEqual(got, wantEdges) {
		t.Fatalf("edges = %v, want %v", got, wantEdges)
	}
}

func TestDocumentValidatesGraphOutputReferences(t *testing.T) {
	doc := minimalValidDocument()
	doc.Tasks["main"].Graph.Output = map[string]any{
		"value": map[string]any{"$from": "node", "node": "missing"},
	}

	_, err := Document(doc, builtin.SchemaRegistry())
	if err == nil || !strings.Contains(err.Error(), `graph.output: node ref "missing" not found`) {
		t.Fatalf("error = %v, want missing graph output reference", err)
	}
}

func TestDocumentIncludesLoopControlNodeReferences(t *testing.T) {
	doc := minimalValidDocument()
	doc.Tasks["body"] = model.TaskDef{
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Graph:        &model.Graph{Nodes: map[string]model.Node{}, Output: map[string]any{}},
	}
	doc.Tasks["main"].Graph.Nodes = map[string]model.Node{
		"seed": {Task: "template.expand"},
		"gate": {Task: "template.expand"},
		"loop": {
			Loop: &model.LoopSpec{
				BodyTask:      "body",
				MaxIterations: 1,
				State: map[string]any{
					"value": map[string]any{"$from": "node", "node": "seed"},
				},
				ContinueWhen: map[string]any{"$from": "node", "node": "gate"},
			},
		},
	}
	doc.Tasks["main"].Graph.Output = map[string]any{"value": map[string]any{"$from": "node", "node": "loop"}}

	verified, err := Document(doc, builtin.SchemaRegistry())
	if err != nil {
		t.Fatal(err)
	}
	want := []Edge{{From: "gate", To: "loop"}, {From: "seed", To: "loop"}}
	if got := verified.Graphs["main"].Edges; !reflect.DeepEqual(got, want) {
		t.Fatalf("edges = %v, want %v", got, want)
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
