package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/builtin"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/cache"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
)

func cacheDoc(cacheValue any) *model.Document {
	inputs := map[string]any{"script": "Write-Output hi"}
	if cacheValue != nil {
		inputs["cache"] = cacheValue
	}
	return &model.Document{
		Kind: model.DocumentKind, Version: model.DocumentVersion, Entry: "main",
		Tasks: map[string]model.TaskDef{"main": {
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			Graph: &model.Graph{
				Nodes: map[string]model.Node{"step": {
					Task: "pwsh.run", Inputs: inputs, OutputSchema: map[string]any{"type": "object"},
				}},
				Output: map[string]any{"result": map[string]any{"$from": "node", "node": "step"}},
			},
		}},
	}
}

func runHostTwice(t *testing.T, doc *model.Document) (calls, secondHits int) {
	t.Helper()
	fake := &provider.FakeProvider{
		ProviderName: provider.NamePwsh,
		Respond: provider.StaticResult(map[string]any{
			provider.PwshOutputStdout: "hi\n", provider.PwshOutputStderr: "", provider.PwshOutputExitCode: 0,
		}),
	}
	store := newTestStore(t, t.TempDir())
	for i, runID := range []string{"run-1", "run-2"} {
		rt := builtin.RuntimeRegistry()
		rt.SetProviders(provider.NewSet(fake))
		eng := New(doc, rt, store, nil)
		if _, err := eng.Run(context.Background(), Options{RunID: runID, Input: map[string]any{}, MaxParallel: 1}); err != nil {
			t.Fatalf("%s: %v", runID, err)
		}
		if i == 1 {
			secondHits = eng.Stats().CacheHits
		}
	}
	return len(fake.Requests()), secondHits
}

func TestHostPwshMemoizesByDefault(t *testing.T) {
	calls, hits := runHostTwice(t, cacheDoc(nil))
	if calls != 1 || hits != 1 {
		t.Fatalf("calls=%d hits=%d, want 1 and 1", calls, hits)
	}
}

func TestHostPwshCacheFalseAlwaysExecutes(t *testing.T) {
	calls, hits := runHostTwice(t, cacheDoc(false))
	if calls != 2 || hits != 0 {
		t.Fatalf("calls=%d hits=%d, want 2 and 0", calls, hits)
	}
}

func TestNestedTaskDoesNotCacheOverNonCacheableChild(t *testing.T) {
	doc := &model.Document{
		Kind: model.DocumentKind, Version: model.DocumentVersion, Entry: "main",
		Tasks: map[string]model.TaskDef{
			"main": {
				InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes:  map[string]model.Node{"nested": {Task: "child", Inputs: map[string]any{}}},
					Output: map[string]any{"result": map[string]any{"$from": "node", "node": "nested"}},
				},
			},
			"child": {
				InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{"step": {
						Task:         "pwsh.run",
						Inputs:       map[string]any{"script": "Write-Output hi", "cache": false},
						OutputSchema: map[string]any{"type": "object"},
					}},
					Output: map[string]any{"result": map[string]any{"$from": "node", "node": "step"}},
				},
			},
		},
	}

	calls, hits := runHostTwice(t, doc)
	if calls != 2 || hits != 0 {
		t.Fatalf("calls=%d hits=%d, want 2 and 0", calls, hits)
	}
}

func TestFileWriteRepairsExternallyRevertedContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.txt")
	if err := os.WriteFile(path, []byte("before"), 0o666); err != nil {
		t.Fatal(err)
	}
	doc := &model.Document{
		Kind: model.DocumentKind, Version: model.DocumentVersion, Entry: "main",
		Tasks: map[string]model.TaskDef{"main": {
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			Graph: &model.Graph{
				Nodes: map[string]model.Node{"write": {
					Task: "file.write", Inputs: map[string]any{"path": path, "content": "desired"},
				}},
				Output: map[string]any{"path": map[string]any{"$from": "node", "node": "write"}},
			},
		}},
	}
	store := newTestStore(t, t.TempDir())
	for _, runID := range []string{"first", "second"} {
		if runID == "second" {
			if err := os.WriteFile(path, []byte("before"), 0o666); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := New(doc, builtin.RuntimeRegistry(), store, nil).Run(context.Background(), Options{
			RunID: runID, Input: map[string]any{}, MaxParallel: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "desired" {
		t.Fatalf("content = %q, want desired", got)
	}
}

// Suppressing memoization must not destabilize identity. A downstream
// memoizable node can hit on the second run only if the predecessor ID is
// stable.
func TestCacheFalseKeepsStableNodeID(t *testing.T) {
	doc := cacheDoc(false)
	graph := doc.Tasks["main"].Graph
	graph.Nodes["after"] = model.Node{
		Task: "template.expand", Inputs: map[string]any{"template": "done", "vars": map[string]any{}},
		DependsOn: []string{"step"}, OutputSchema: map[string]any{"type": "string"},
	}
	graph.Output = map[string]any{"msg": map[string]any{"$from": "node", "node": "after"}}

	_, hits := runHostTwice(t, doc)
	if hits != 1 {
		t.Fatalf("second-run hits=%d, want downstream hit", hits)
	}
}

func TestNodeIDIsPassedToTasks(t *testing.T) {
	doc := &model.Document{
		Kind: model.DocumentKind, Version: model.DocumentVersion, Entry: "main",
		Tasks: map[string]model.TaskDef{"main": {
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			Graph: &model.Graph{
				Nodes:  map[string]model.Node{"probe": {Task: "capture.nodeid", Inputs: map[string]any{}}},
				Output: map[string]any{"seen": map[string]any{"$from": "node", "node": "probe"}},
			},
		}},
	}
	rt := builtin.RuntimeRegistry()
	rt.Register("capture.nodeid", func(_ context.Context, _ map[string]any, c builtin.Context) (any, error) {
		return map[string]any{"nodeId": c.NodeID}, nil
	})
	out, err := New(doc, rt, newTestStore(t, t.TempDir()), nil).Run(context.Background(), Options{
		RunID: "run-nodeid", Input: map[string]any{}, MaxParallel: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := out.(map[string]any)["seen"].(map[string]any)["nodeId"]
	if got, ok := seen.(string); !ok || got == "" {
		t.Fatalf("task received nodeId %v, want non-empty", seen)
	}
}

func TestLeaseBoundTransientRunNeverMemoizes(t *testing.T) {
	doc := devLoopDoc()
	tests := doc.Tasks["main"].Graph.Nodes["tests"]
	tests.Inputs["script"] = "Invoke-Tests"
	doc.Tasks["main"].Graph.Nodes["tests"] = tests
	store := cache.New(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run-1", "run-2"} {
		fake := &target.FakeBackend{}
		runDevLoop(t, doc, fake, store, runID)
		found := false
		for _, exec := range fake.Runs() {
			if exec.Script == "Invoke-Tests" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: transient lease-bound run was served from cache", runID)
		}
	}
}
