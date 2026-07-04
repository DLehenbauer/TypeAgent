package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/builtin"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/cache"
	tflog "github.com/microsoft/TypeAgent/go/taskpilot/internal/logging"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/telemetry"
)

func TestRunTemplateWorkflow(t *testing.T) {
	doc := &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "hello",
		Tasks: map[string]model.TaskDef{
			"hello": {
				InputSchema:  map[string]any{"type": "object", "required": []any{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string"}}},
				OutputSchema: map[string]any{"type": "object", "required": []any{"message"}, "properties": map[string]any{"message": map[string]any{"type": "string"}}},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						"render": {
							Task: "template.expand",
							Inputs: map[string]any{
								"template": "Hello, {{ name }}!",
								"vars": map[string]any{
									"name": map[string]any{"$from": "input", "name": "name"},
								},
							},
							OutputSchema: map[string]any{"type": "string"},
						},
					},
					Output: map[string]any{"message": map[string]any{"$from": "node", "node": "render"}},
				},
			},
		},
	}
	out := runTestDoc(t, doc, "run-test", map[string]any{"name": "world"}, 2)
	got := out.(map[string]any)["message"]
	if got != "Hello, world!" {
		t.Fatalf("message = %v", got)
	}
}

func TestRunAppliesInputSchemaDefaults(t *testing.T) {
	doc := &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "hello",
		Tasks: map[string]model.TaskDef{
			"hello": {
				InputSchema: map[string]any{
					"type":     "object",
					"required": []any{"name"},
					"properties": map[string]any{
						"name": map[string]any{"type": "string", "default": "world"},
					},
				},
				OutputSchema: map[string]any{"type": "object", "required": []any{"message"}, "properties": map[string]any{"message": map[string]any{"type": "string"}}},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						"render": {
							Task: "template.expand",
							Inputs: map[string]any{
								"template": "Hello, {{ name }}!",
								"vars": map[string]any{
									"name": map[string]any{"$from": "input", "name": "name"},
								},
							},
							OutputSchema: map[string]any{"type": "string"},
						},
					},
					Output: map[string]any{"message": map[string]any{"$from": "node", "node": "render"}},
				},
			},
		},
	}
	// Omit the required "name": its schema default should satisfy validation
	// and flow into the node.
	out := runTestDoc(t, doc, "run-defaults", map[string]any{}, 1)
	if got := out.(map[string]any)["message"]; got != "Hello, world!" {
		t.Fatalf("message = %v, want default applied", got)
	}
}

func TestEngineStatsCountsCacheHitsAndMisses(t *testing.T) {
	doc := &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "main",
		Tasks: map[string]model.TaskDef{
			"main": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						"greet": {
							Task:         "template.expand",
							Inputs:       map[string]any{"template": "hello", "vars": map[string]any{}},
							OutputSchema: map[string]any{"type": "string"},
						},
					},
					Output: map[string]any{"msg": map[string]any{"$from": "node", "node": "greet"}},
				},
			},
		},
	}
	rt := builtin.RuntimeRegistry()
	store := newTestStore(t, t.TempDir())

	// First run executes the node: one cacheable miss, no hits.
	first := New(doc, rt, store, nil)
	if _, err := first.Run(context.Background(), Options{RunID: "run-miss", Input: map[string]any{}, MaxParallel: 1}); err != nil {
		t.Fatal(err)
	}
	if s := first.Stats(); s.CacheMisses != 1 || s.CacheHits != 0 {
		t.Fatalf("first run stats = %+v, want 1 miss / 0 hits", s)
	}

	// Second run over the same store serves the node from cache: one hit.
	second := New(doc, rt, store, nil)
	if _, err := second.Run(context.Background(), Options{RunID: "run-hit", Input: map[string]any{}, MaxParallel: 1}); err != nil {
		t.Fatal(err)
	}
	if s := second.Stats(); s.CacheHits != 1 || s.CacheMisses != 0 {
		t.Fatalf("second run stats = %+v, want 1 hit / 0 misses", s)
	}
}

func TestFileRefCacheIsContentAddressed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "src.txt")
	if err := os.WriteFile(file, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	doc := &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "main",
		Tasks: map[string]model.TaskDef{
			"main": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						"ref": {
							Task:         "file.ref",
							Inputs:       map[string]any{"path": file},
							OutputSchema: map[string]any{"type": "object"},
						},
					},
					Output: map[string]any{"fingerprint": map[string]any{"$from": "node", "node": "ref", "path": []any{"$file", "fingerprint"}}},
				},
			},
		},
	}
	rt := builtin.RuntimeRegistry()
	store := newTestStore(t, t.TempDir())

	// First run fingerprints and caches: one miss, fresh fingerprint.
	first := New(doc, rt, store, nil)
	out, err := first.Run(context.Background(), Options{RunID: "r1", Input: map[string]any{}, MaxParallel: 1})
	if err != nil {
		t.Fatal(err)
	}
	fpFirst, _ := out.(map[string]any)["fingerprint"].(string)
	if fpFirst == "" {
		t.Fatalf("fingerprint = %v, want non-empty", out)
	}
	if s := first.Stats(); s.CacheMisses != 1 || s.CacheHits != 0 {
		t.Fatalf("first run stats = %+v, want 1 miss / 0 hits", s)
	}

	// Unchanged content serves from cache: one hit, no execution.
	unchanged := New(doc, rt, store, nil)
	if _, err := unchanged.Run(context.Background(), Options{RunID: "r2", Input: map[string]any{}, MaxParallel: 1}); err != nil {
		t.Fatal(err)
	}
	if s := unchanged.Stats(); s.CacheHits != 1 || s.CacheMisses != 0 {
		t.Fatalf("unchanged run stats = %+v, want 1 hit / 0 misses", s)
	}

	// Editing the file invalidates the cache: one miss, fresh fingerprint.
	if err := os.WriteFile(file, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := New(doc, rt, store, nil)
	out, err = changed.Run(context.Background(), Options{RunID: "r3", Input: map[string]any{}, MaxParallel: 1})
	if err != nil {
		t.Fatal(err)
	}
	fpSecond, _ := out.(map[string]any)["fingerprint"].(string)
	if fpSecond == fpFirst {
		t.Fatalf("fingerprint after edit = %v, want different from %v", fpSecond, fpFirst)
	}
	if s := changed.Stats(); s.CacheMisses != 1 || s.CacheHits != 0 {
		t.Fatalf("changed run stats = %+v, want 1 miss / 0 hits", s)
	}
}

func TestRunUsesInjectedProvider(t *testing.T) {
	doc := &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "main",
		Tasks: map[string]model.TaskDef{
			"main": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						"build": {
							Task: "pwsh.run",
							Inputs: map[string]any{
								"script": "Write-Output hi",
							},
							OutputSchema: map[string]any{"type": "object"},
						},
					},
					Output: map[string]any{"result": map[string]any{"$from": "node", "node": "build"}},
				},
			},
		},
	}

	fake := &provider.FakeProvider{
		ProviderName: "pwsh",
		Respond:      provider.StaticResult(map[string]any{"stdout": "hi\n", "stderr": "", "exitCode": 0}),
	}
	rt := builtin.RuntimeRegistry()
	rt.SetProviders(provider.NewSet(fake))

	store := newTestStore(t, t.TempDir())
	out, err := New(doc, rt, store, nil).Run(context.Background(), Options{
		RunID:       "run-provider",
		Input:       map[string]any{},
		MaxParallel: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := out.(map[string]any)["result"].(map[string]any)
	if result["stdout"] != "hi\n" {
		t.Fatalf("stdout = %v, want hi", result["stdout"])
	}

	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider received %d requests, want 1", len(reqs))
	}
	if reqs[0].Input["script"] != "Write-Output hi" {
		t.Fatalf("provider request script = %v", reqs[0].Input["script"])
	}
}

func TestFailedResultIsNotCached(t *testing.T) {
	doc := &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "main",
		Tasks: map[string]model.TaskDef{
			"main": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						"build": {
							Task:         "pwsh.run",
							Inputs:       map[string]any{"script": "build.ps1"},
							OutputSchema: map[string]any{"type": "object"},
						},
					},
					Output: map[string]any{"result": map[string]any{"$from": "node", "node": "build"}},
				},
			},
		},
	}

	calls := 0
	var callsMu sync.Mutex
	fake := &provider.FakeProvider{
		ProviderName: "pwsh",
		Respond: func(provider.Request) (provider.Result, error) {
			callsMu.Lock()
			calls++
			n := calls
			callsMu.Unlock()
			if n == 1 {
				return map[string]any{"stdout": "", "stderr": "boom", "exitCode": 1}, nil
			}
			return map[string]any{"stdout": "ok", "stderr": "", "exitCode": 0}, nil
		},
	}
	rt := builtin.RuntimeRegistry()
	rt.SetProviders(provider.NewSet(fake))

	// A shared cache across both runs: if the failed first run were cached, the
	// second run would hit it instead of re-executing.
	store := newTestStore(t, t.TempDir())

	if _, err := New(doc, rt, store, nil).Run(context.Background(), Options{
		RunID: "run-fail", Input: map[string]any{}, MaxParallel: 1,
	}); err == nil {
		t.Fatal("expected first run to fail on non-zero exit")
	}

	out, err := New(doc, rt, store, nil).Run(context.Background(), Options{
		RunID: "run-ok", Input: map[string]any{}, MaxParallel: 1,
	})
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	callsMu.Lock()
	got := calls
	callsMu.Unlock()
	if got != 2 {
		t.Fatalf("provider calls = %d, want 2 (failure must not be cached)", got)
	}
	result := out.(map[string]any)["result"].(map[string]any)
	if result["exitCode"] != 0 {
		t.Fatalf("second run exitCode = %v, want 0", result["exitCode"])
	}
}

func TestRunForEachWorkflow(t *testing.T) {
	doc := &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "main",
		Tasks: map[string]model.TaskDef{
			"main": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						"reviewEach": {
							Task: "template.expand",
							ForEach: &model.ForEachSpec{
								Items: []any{
									map[string]any{"name": "one"},
									map[string]any{"name": "two"},
								},
								MaxConcurrency: 2,
							},
							Inputs: map[string]any{
								"template": "{{ name }}",
								"vars": map[string]any{
									"name": map[string]any{"$from": "item", "path": []any{"name"}},
								},
							},
							OutputSchema: map[string]any{"type": "string"},
						},
					},
					Output: map[string]any{"items": map[string]any{"$from": "node", "node": "reviewEach"}},
				},
			},
		},
	}
	out := runTestDoc(t, doc, "run-map", map[string]any{}, 4)
	items := out.(map[string]any)["items"].([]any)
	if got := items[0]; got != "one" {
		t.Fatalf("items[0] = %v", got)
	}
	if got := items[1]; got != "two" {
		t.Fatalf("items[1] = %v", got)
	}
}

// TestForEachClaimWaitDoesNotDeadlock reproduces the cache-claim / concurrency
// deadlock and proves the engine no longer hangs. A forEach fans out items that
// all dedupe onto one shared cache node; with a small concurrency limit, the
// workers waiting for that shared node used to hold every pool slot, so the
// worker that must compute it could never be scheduled -- the run pegged the CPU
// and never progressed. Each item also runs a distinct "marker" node, and the
// shared node blocks until every item has arrived. That can only happen if the
// claim-waiters release their concurrency slots while parked, which is exactly
// the fix.
func TestForEachClaimWaitDoesNotDeadlock(t *testing.T) {
	const itemCount = 4

	var arrived int32
	allArrived := make(chan struct{})
	var once sync.Once
	fake := &provider.FakeProvider{
		ProviderName: "pwsh",
		Respond: func(req provider.Request) (provider.Result, error) {
			if req.Input["script"] == "marker" {
				if atomic.AddInt32(&arrived, 1) == itemCount {
					once.Do(func() { close(allArrived) })
				}
				return map[string]any{"stdout": "", "stderr": "", "exitCode": 0}, nil
			}
			// The single deduped computation. Block until every item has reached
			// it; if the waiters never release their slots this never fires and
			// the safety timeout surfaces the deadlock as a failed node.
			select {
			case <-allArrived:
				return map[string]any{"stdout": "shared", "stderr": "", "exitCode": 0}, nil
			case <-time.After(15 * time.Second):
				return map[string]any{"stdout": "", "stderr": "items never all arrived: deadlock", "exitCode": 1}, nil
			}
		},
	}
	rt := builtin.RuntimeRegistry()
	rt.SetProviders(provider.NewSet(fake))

	store := newTestStore(t, t.TempDir())

	doc := &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "main",
		Tasks: map[string]model.TaskDef{
			"main": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						"fan": {
							Task: "leaf",
							ForEach: &model.ForEachSpec{
								Items:          []any{0, 1, 2, 3},
								MaxConcurrency: 2,
							},
							Inputs: map[string]any{
								"idx": map[string]any{"$from": "item"},
							},
						},
					},
					Output: map[string]any{"done": map[string]any{"$from": "node", "node": "fan"}},
				},
			},
			"leaf": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						// Distinct per item (args carry idx), so it runs for every
						// item and bumps the arrival counter.
						"marker": {
							Task: "pwsh.run",
							Inputs: map[string]any{
								"script": "marker",
								"args":   []any{map[string]any{"$from": "input", "name": "idx"}},
							},
							OutputSchema: map[string]any{"type": "object"},
						},
						// Identical across items (no item-dependent input or
						// predecessor), so all items dedupe onto one cache claim.
						"shared": {
							Task: "pwsh.run",
							Inputs: map[string]any{
								"script": "shared",
							},
							OutputSchema: map[string]any{"type": "object"},
						},
					},
					Output: map[string]any{
						"m": map[string]any{"$from": "node", "node": "marker"},
						"s": map[string]any{"$from": "node", "node": "shared"},
					},
				},
			},
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := New(doc, rt, store, nil).Run(context.Background(), Options{
			RunID:       "run-claim-deadlock",
			Input:       map[string]any{},
			MaxParallel: 2,
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run failed (deadlock not resolved): %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not complete within 20s: forEach claim-wait deadlock")
	}
}

func TestRunLoopWorkflow(t *testing.T) {
	doc := &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "main",
		Tasks: map[string]model.TaskDef{
			"main": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						"validate": {
							Loop: &model.LoopSpec{
								MaxIterations: 3,
								BodyTask:      "body",
								Inputs:        map[string]any{},
								ContinueWhen:  map[string]any{"$from": "body", "path": []any{"shouldContinue"}},
							},
						},
					},
					Output: map[string]any{"validation": map[string]any{"$from": "node", "node": "validate"}},
				},
			},
			"body": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{},
					Output: map[string]any{
						"shouldContinue": false,
						"passed":         true,
					},
				},
			},
		},
	}
	out := runTestDoc(t, doc, "run-loop", map[string]any{}, 2)
	validation := out.(map[string]any)["validation"].(map[string]any)
	if validation["passed"] != true {
		t.Fatalf("validation.passed = %v", validation["passed"])
	}
}

func TestRunLogsCacheRefOnCompletedNode(t *testing.T) {
	doc := &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "hello",
		Tasks: map[string]model.TaskDef{
			"hello": {
				InputSchema:  map[string]any{"type": "object", "required": []any{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string"}}},
				OutputSchema: map[string]any{"type": "object", "required": []any{"message"}, "properties": map[string]any{"message": map[string]any{"type": "string"}}},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						"render": {
							Task: "template.expand",
							Inputs: map[string]any{
								"template": "Hello, {{ name }}!",
								"vars": map[string]any{
									"name": map[string]any{"$from": "input", "name": "name"},
								},
							},
							OutputSchema: map[string]any{"type": "string"},
						},
					},
					Output: map[string]any{"message": map[string]any{"$from": "node", "node": "render"}},
				},
			},
		},
	}
	temp := t.TempDir()
	store := newTestStore(t, temp)
	logger, err := tflog.Open(filepath.Join(temp, "logs"), "run-log")
	if err != nil {
		t.Fatal(err)
	}
	tp, shutdown := telemetry.Setup(logger)
	if _, err := New(doc, builtin.RuntimeRegistry(), store, tp).Run(context.Background(), Options{
		RunID:       "run-log",
		Input:       map[string]any{"name": "world"},
		MaxParallel: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	records := readSpanRecords(t, tflog.LogPath(filepath.Join(temp, "logs"), "run-log"))
	var completed *telemetry.SpanRecord
	for i := range records {
		r := records[i]
		if r.Phase == telemetry.PhaseEnd && attrString(r, telemetry.AttrSpanKind) == telemetry.SpanKindNode && attrString(r, telemetry.AttrNodeName) == "render" {
			completed = &records[i]
			break
		}
	}
	if completed == nil {
		t.Fatal("completed span for render not found")
	}
	if completed.EndTime == nil {
		t.Fatal("completed span missing end time / duration")
	}
	if completed.Status != telemetry.StatusOK {
		t.Fatalf("completed span status = %q, want ok", completed.Status)
	}
	if got := attrString(*completed, telemetry.AttrCacheStatus); got != telemetry.CacheStatusMiss {
		t.Fatalf("cache status = %q, want %q", got, telemetry.CacheStatusMiss)
	}
	cachePath := attrString(*completed, telemetry.AttrCachePath)
	if cachePath == "" {
		t.Fatal("cache path attribute missing")
	}
	if _, err := os.Stat(filepath.Join(temp, cachePath)); err != nil {
		t.Fatalf("cache ref path not readable: %v", err)
	}
}

func attrString(rec telemetry.SpanRecord, key string) string {
	if v, ok := rec.Attributes[key]; ok {
		return v.String()
	}
	return ""
}

func readSpanRecords(t *testing.T, path string) []telemetry.SpanRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records, err := tflog.ReadSpanRecords(f)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// newTestStore builds and initializes a cache store rooted under dir, failing
// the test if initialization errors.
func newTestStore(t *testing.T, dir string) *cache.Store {
	t.Helper()
	store := cache.New(filepath.Join(dir, "cache"))
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	return store
}

func runTestDoc(t *testing.T, doc *model.Document, runID string, input map[string]any, maxParallel int) any {
	t.Helper()
	store := newTestStore(t, t.TempDir())
	out, err := New(doc, builtin.RuntimeRegistry(), store, nil).Run(context.Background(), Options{
		RunID:       runID,
		Input:       input,
		MaxParallel: maxParallel,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// concurrencyRecorder records the peak number of concurrent provider Submit
// executions, so a test can prove the provider's own concurrency cap bounds a
// fan-out regardless of the engine's MaxParallel. Its respond method plugs into
// a FakeProvider registered under the pwsh name.
type concurrencyRecorder struct {
	running int32
	maxSeen int32
}

func (c *concurrencyRecorder) respond(provider.Request) (provider.Result, error) {
	cur := atomic.AddInt32(&c.running, 1)
	for {
		m := atomic.LoadInt32(&c.maxSeen)
		if cur <= m || atomic.CompareAndSwapInt32(&c.maxSeen, m, cur) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	atomic.AddInt32(&c.running, -1)
	return map[string]any{provider.PwshOutputExitCode: 0}, nil
}

// TestProviderCapBoundsFanOut verifies that a provider's concurrency cap is the
// real throttle for the graph's implicit fan-out: even with a wide engine
// MaxParallel and no forEach.maxConcurrency, a provider limited to N never runs
// more than N work items at once.
func TestProviderCapBoundsFanOut(t *testing.T) {
	const items = 8
	const providerLimit = 2

	itemList := make([]any, items)
	for i := range itemList {
		itemList[i] = map[string]any{"script": "echo " + strconv.Itoa(i)}
	}
	doc := &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "main",
		Tasks: map[string]model.TaskDef{
			"main": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						"fan": {
							Task:    "pwsh.run",
							ForEach: &model.ForEachSpec{Items: itemList},
							Inputs: map[string]any{
								"script": map[string]any{
									"$from": "item",
									"path":  []any{"script"},
								},
							},
							OutputSchema: map[string]any{"type": "object"},
						},
					},
					Output: map[string]any{"items": map[string]any{"$from": "node", "node": "fan"}},
				},
			},
		},
	}

	recorder := &concurrencyRecorder{}
	rt := builtin.RuntimeRegistry()
	rt.SetProviders(provider.NewSet(&provider.FakeProvider{
		ProviderName: provider.NamePwsh,
		Limit:        providerLimit,
		Respond:      recorder.respond,
	}))

	store := newTestStore(t, t.TempDir())
	// MaxParallel is wide (>= items) so the engine does not throttle the
	// fan-out; the provider cap must.
	out, err := New(doc, rt, store, nil).Run(context.Background(), Options{
		RunID:       "run-fanout",
		Input:       map[string]any{},
		MaxParallel: items * 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(out.(map[string]any)["items"].([]any)); got != items {
		t.Fatalf("produced %d items, want %d", got, items)
	}
	peak := atomic.LoadInt32(&recorder.maxSeen)
	if peak > providerLimit {
		t.Fatalf("observed %d concurrent provider runs, want <= %d", peak, providerLimit)
	}
	if peak < providerLimit {
		t.Fatalf("observed peak concurrency %d; expected the fan-out to reach the cap of %d", peak, providerLimit)
	}
}

// forEachConcurrencyDoc builds a single-node forEach graph whose fan-out
// concurrency is driven by the given forEach.maxConcurrency and node-level
// maxConcurrency, so a test can exercise the engine's concurrency validation.
func forEachConcurrencyDoc(forEachMax any, nodeMax int) *model.Document {
	return &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "main",
		Tasks: map[string]model.TaskDef{
			"main": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph: &model.Graph{
					Nodes: map[string]model.Node{
						"fan": {
							Task:           "pwsh.run",
							MaxConcurrency: nodeMax,
							ForEach: &model.ForEachSpec{
								Items:          []any{map[string]any{"script": "echo 0"}},
								MaxConcurrency: forEachMax,
							},
							Inputs: map[string]any{
								"script": map[string]any{"$from": "item", "path": []any{"script"}},
							},
							OutputSchema: map[string]any{"type": "object"},
						},
					},
					Output: map[string]any{"items": map[string]any{"$from": "node", "node": "fan"}},
				},
			},
		},
	}
}

// TestForEachConcurrencyValidation proves invalid forEach/maxConcurrency
// settings fail with an explicit boundary error rather than being silently
// coerced to a default.
func TestForEachConcurrencyValidation(t *testing.T) {
	cases := []struct {
		name       string
		forEachMax any
		nodeMax    int
		wantErr    string
	}{
		{name: "forEach non-integer", forEachMax: "two", wantErr: "forEach.maxConcurrency must resolve to an integer"},
		{name: "forEach fractional", forEachMax: 1.5, wantErr: "forEach.maxConcurrency must resolve to an integer"},
		{name: "forEach zero", forEachMax: 0, wantErr: "forEach.maxConcurrency must be a positive integer"},
		{name: "forEach negative", forEachMax: -3, wantErr: "forEach.maxConcurrency must be a positive integer"},
		{name: "node negative", nodeMax: -1, wantErr: "maxConcurrency must be a positive integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := forEachConcurrencyDoc(tc.forEachMax, tc.nodeMax)
			store := newTestStore(t, t.TempDir())
			_, err := New(doc, builtin.RuntimeRegistry(), store, nil).Run(context.Background(), Options{
				RunID:       "run-concurrency",
				Input:       map[string]any{},
				MaxParallel: 4,
			})
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestForEachConcurrencyValidAccepted proves a positive forEach.maxConcurrency
// and node maxConcurrency still run the fan-out successfully.
func TestForEachConcurrencyValidAccepted(t *testing.T) {
	doc := forEachConcurrencyDoc(2, 3)
	rt := builtin.RuntimeRegistry()
	rt.SetProviders(provider.NewSet(&provider.FakeProvider{
		ProviderName: provider.NamePwsh,
		Respond:      provider.StaticResult(map[string]any{provider.PwshOutputExitCode: 0}),
	}))
	store := newTestStore(t, t.TempDir())
	if _, err := New(doc, rt, store, nil).Run(context.Background(), Options{
		RunID:       "run-concurrency-ok",
		Input:       map[string]any{},
		MaxParallel: 4,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
