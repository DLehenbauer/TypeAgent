package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/builtin"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/cache"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/canonical"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/plan"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/schema"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Options configures a single Run. RunID labels the run for tracing and cache
// keying; if empty, Run generates one via NewRunID. Input is the entry task's
// input payload. DryRun skips side-effecting work and disables caching.
// MaxParallel bounds concurrent node execution and must be at least 1; Run
// rejects non-positive values.
type Options struct {
	RunID       string
	Input       map[string]any
	DryRun      bool
	MaxParallel int
}

// Engine executes a parsed document's task graph against a runtime, reusing a
// cache and emitting trace spans. Construct one with New and reuse it across
// runs; per-run cache counters are reset at the start of each Run.
type Engine struct {
	doc       *model.Document
	runtime   *builtin.Runtime
	cache     *cache.Store
	validator *schema.Validator
	tracer    trace.Tracer

	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
}

// Stats summarizes cache effectiveness for a completed (or failed) run. It
// counts only cacheable nodes: a hit is a node served from the cache, a miss is
// a cacheable node that had to execute. Dry-run and non-cacheable nodes (those
// keyed on the run id) are excluded from both counts.
type Stats struct {
	CacheHits   int
	CacheMisses int
}

// Stats returns the cache hit/miss tally accumulated since the last Run call.
func (e *Engine) Stats() Stats {
	return Stats{
		CacheHits:   int(e.cacheHits.Load()),
		CacheMisses: int(e.cacheMisses.Load()),
	}
}

// New builds an Engine. doc, runtime, and store are required and must be
// non-nil; passing nil for any of them panics, since an Engine in that state
// cannot run. tp supplies the tracer used to emit run/node spans; pass nil to
// disable tracing (a no-op provider is used).
func New(doc *model.Document, runtime *builtin.Runtime, store *cache.Store, tp trace.TracerProvider) *Engine {
	if doc == nil {
		panic("engine: New requires a non-nil doc")
	}
	if runtime == nil {
		panic("engine: New requires a non-nil runtime")
	}
	if store == nil {
		panic("engine: New requires a non-nil store")
	}
	if tp == nil {
		tp = noop.NewTracerProvider()
	}
	return &Engine{doc: doc, runtime: runtime, cache: store, validator: schema.NewValidator(), tracer: tp.Tracer(telemetry.TracerName)}
}

// NewRunID returns a unique run identifier of the form "run-<hex>", using 16
// bytes of crypto-random data. If randomness is unavailable, it falls back to a
// timestamp-based id.
func NewRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b[:])
}

// Run executes the document's entry task and returns its output. A zero RunID
// is filled in via NewRunID and MaxParallel must be at least 1 or Run returns a
// boundary error. Per-run cache counters are reset so Stats reflects only this
// run, and a root trace span wraps the execution. Returns the entry task's
// output or the first error encountered.
func (e *Engine) Run(ctx context.Context, opts Options) (any, error) {
	if opts.MaxParallel < 1 {
		return nil, fmt.Errorf("MaxParallel must be at least 1, got %d", opts.MaxParallel)
	}
	if opts.RunID == "" {
		opts.RunID = NewRunID()
	}
	// Reset per-run cache counters so Stats() reflects only this run.
	e.cacheHits.Store(0)
	e.cacheMisses.Store(0)
	ctx, span := e.tracer.Start(ctx, e.doc.Entry, trace.WithAttributes(
		attribute.String(telemetry.AttrSpanKind, telemetry.SpanKindRun),
		attribute.String(telemetry.AttrRunID, opts.RunID),
		attribute.String(telemetry.AttrEntry, e.doc.Entry),
		attribute.Bool(telemetry.AttrDryRun, opts.DryRun),
	))
	defer span.End()
	out, err := e.runTask(ctx, e.doc.Entry, opts.Input, opts)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

// runTask applies input defaults, validates the task contract, runs the graph,
// and validates the task output.
func (e *Engine) runTask(ctx context.Context, taskName string, input map[string]any, opts Options) (any, error) {
	task, ok := e.doc.Tasks[taskName]
	if !ok {
		return nil, fmt.Errorf("task %q not found", taskName)
	}
	input = schema.ApplyDefaults(task.InputSchema, input)
	if err := e.validator.Validate(task.InputSchema, input); err != nil {
		return nil, fmt.Errorf("%s input schema: %w", taskName, err)
	}
	out, err := e.runGraph(ctx, taskName, task.Graph, input, opts)
	if err != nil {
		return nil, err
	}
	if err := e.validator.Validate(task.OutputSchema, out); err != nil {
		return nil, fmt.Errorf("%s output schema: %w", taskName, err)
	}
	return out, nil
}

// runGraph evaluates a task graph in dependency waves, caps each ready wave
// with MaxParallel, then resolves the graph output.
func (e *Engine) runGraph(ctx context.Context, taskName string, graph *model.Graph, input map[string]any, opts Options) (any, error) {
	if graph == nil {
		return nil, fmt.Errorf("task %s has no graph", taskName)
	}
	constants := map[string]any{}
	for k, c := range e.doc.Constants {
		constants[k] = c.Value
	}
	scope := &scopeState{
		input:     input,
		constants: constants,
		nodes:     map[string]nodeResult{},
		runID:     opts.RunID,
	}
	if len(graph.Nodes) == 0 {
		return resolveTemplate(graph.Output, scope)
	}
	deps := buildDeps(graph)
	done := map[string]bool{}
	for len(done) < len(graph.Nodes) {
		ready := readyNodes(graph, deps, done)
		if len(ready) == 0 {
			return nil, fmt.Errorf("graph %s has no ready nodes; verify should have rejected this", taskName)
		}
		// Start the ready wave while capping graph-level concurrency.
		sem := make(chan struct{}, opts.MaxParallel)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var firstErr error
		for _, id := range ready {
			id := id
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				holder := &slotHolder{sem: sem}
				defer holder.release()
				result, err := e.runNode(withConcurrencySlot(ctx, holder), id, graph.Nodes[id], deps[id], scope, opts)
				mu.Lock()
				defer mu.Unlock()
				if err != nil && firstErr == nil {
					firstErr = err
					return
				}
				scope.nodes[id] = result
				done[id] = true
			}()
		}
		wg.Wait()
		if firstErr != nil {
			return nil, firstErr
		}
	}
	return resolveTemplate(graph.Output, scope)
}

// buildDeps combines declared dependencies with node references found in
// templates.
func buildDeps(graph *model.Graph) map[string][]string {
	out := map[string][]string{}
	for id, node := range graph.Nodes {
		set := map[string]bool{}
		for _, dep := range node.DependsOn {
			set[dep] = true
		}
		for _, ref := range collectRefs(node.Inputs) {
			set[ref] = true
		}
		if node.ForEach != nil {
			for _, tf := range node.ForEach.RefTemplates() {
				for _, ref := range collectRefs(tf.Value) {
					set[ref] = true
				}
			}
		}
		if node.Loop != nil {
			for _, tf := range node.Loop.RefTemplates() {
				for _, ref := range collectRefs(tf.Value) {
					set[ref] = true
				}
			}
		}
		for dep := range set {
			out[id] = append(out[id], dep)
		}
		sort.Strings(out[id])
	}
	return out
}

// readyNodes returns unfinished nodes whose dependencies have all completed.
func readyNodes(graph *model.Graph, deps map[string][]string, done map[string]bool) []string {
	var ready []string
	for id := range graph.Nodes {
		if done[id] {
			continue
		}
		ok := true
		for _, dep := range deps[id] {
			if !done[dep] {
				ok = false
				break
			}
		}
		if ok {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	return ready
}

// spanLabel builds a low-cardinality, human-readable span name from a node's
// base name and its task, e.g. "audit (copilot.invoke)". A blank task yields
// just the name.
func spanLabel(name, task string) string {
	if task == "" {
		return name
	}
	return name + " (" + task + ")"
}

// splitForEachID separates a forEach item ID such as "name[3]" into its base
// node name and 0-based item index.
func splitForEachID(id string) (base string, index int, ok bool) {
	if !strings.HasSuffix(id, "]") {
		return id, 0, false
	}
	open := strings.LastIndexByte(id, '[')
	if open <= 0 {
		return id, 0, false
	}
	n, err := strconv.Atoi(id[open+1 : len(id)-1])
	if err != nil {
		return id, 0, false
	}
	return id[:open], n, true
}

// runNode dispatches a node to the execution path for its verified mode.
func (e *Engine) runNode(ctx context.Context, id string, node model.Node, deps []string, scope *scopeState, opts Options) (nodeResult, error) {
	switch node.Mode() {
	case model.NodeModeLoop:
		return e.runLoopNode(ctx, id, node, deps, scope, opts)
	case model.NodeModeForEach:
		return e.runForEachNode(ctx, id, node, deps, scope, opts)
	default:
		return e.runSingleNode(ctx, id, node, deps, scope, opts)
	}
}

// concurrencySlotsKey stores the bounded-concurrency slots occupied by the
// current execution path.
type concurrencySlotsKey struct{}

// slotHolder owns one semaphore token for a worker subtree. Park/unpark are
// reference-counted so multiple blocked descendants can share that token safely.
type slotHolder struct {
	sem      chan struct{}
	mu       sync.Mutex
	parked   int
	tokenOut bool
}

// park frees this holder's token when the first descendant blocks on a cache
// claim.
func (h *slotHolder) park() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.parked++
	if h.parked == 1 {
		<-h.sem
		h.tokenOut = true
	}
}

// unpark reoccupies this holder's token after the last blocked descendant
// resumes.
func (h *slotHolder) unpark() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.parked--
	if h.parked == 0 {
		h.sem <- struct{}{}
		h.tokenOut = false
	}
}

// release permanently returns the token when the owning goroutine exits.
func (h *slotHolder) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.tokenOut {
		<-h.sem
	}
}

// withConcurrencySlot appends holder to the current path's slot stack.
func withConcurrencySlot(ctx context.Context, holder *slotHolder) context.Context {
	prev, _ := ctx.Value(concurrencySlotsKey{}).([]*slotHolder)
	next := make([]*slotHolder, len(prev)+1)
	copy(next, prev)
	next[len(prev)] = holder
	return context.WithValue(ctx, concurrencySlotsKey{}, next)
}

// heldConcurrencySlots returns the current path's slots, outermost first.
func heldConcurrencySlots(ctx context.Context) []*slotHolder {
	slots, _ := ctx.Value(concurrencySlotsKey{}).([]*slotHolder)
	return slots
}

// runForEachNode resolves the item list, runs each item under the effective
// fan-out limit, and returns an aggregate node ID and output.
func (e *Engine) runForEachNode(ctx context.Context, id string, node model.Node, deps []string, scope *scopeState, opts Options) (out nodeResult, err error) {
	ctx, span := e.tracer.Start(ctx, spanLabel(id, "forEach "+node.Task), trace.WithAttributes(
		attribute.String(telemetry.AttrSpanKind, telemetry.SpanKindForEach),
		attribute.String(telemetry.AttrRunID, opts.RunID),
		attribute.String(telemetry.AttrNodeName, id),
		attribute.String(telemetry.AttrTask, node.Task),
	))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}()
	itemsValue, err := resolveTemplate(node.ForEach.Items, scope)
	if err != nil {
		return nodeResult{}, fmt.Errorf("node %s forEach.items: %w", id, err)
	}
	items, ok := itemsValue.([]any)
	if !ok {
		return nodeResult{}, fmt.Errorf("node %s forEach.items must resolve to an array", id)
	}
	span.SetAttributes(attribute.Int(telemetry.AttrForEachCount, len(items)))
	limit := opts.MaxParallel
	if node.ForEach.MaxConcurrency != nil {
		v, err := resolveTemplate(node.ForEach.MaxConcurrency, scope)
		if err != nil {
			return nodeResult{}, fmt.Errorf("node %s forEach.maxConcurrency: %w", id, err)
		}
		n, ok := intValue(v)
		if !ok {
			return nodeResult{}, fmt.Errorf("node %s forEach.maxConcurrency must resolve to an integer, got %v", id, v)
		}
		if n < 1 {
			return nodeResult{}, fmt.Errorf("node %s forEach.maxConcurrency must be a positive integer, got %d", id, n)
		}
		limit = n
	}
	if node.MaxConcurrency < 0 {
		return nodeResult{}, fmt.Errorf("node %s maxConcurrency must be a positive integer, got %d", id, node.MaxConcurrency)
	}
	if node.MaxConcurrency > 0 && node.MaxConcurrency < limit {
		limit = node.MaxConcurrency
	}

	outputs := make([]any, len(items))
	nodeIDs := make([]string, len(items))
	// Each item runs in a worker while preserving the effective fan-out cap.
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i, item := range items {
		i, item := i, item
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			holder := &slotHolder{sem: sem}
			defer holder.release()
			itemScope := scope.withItem(item, i)
			result, err := e.runSingleNode(withConcurrencySlot(ctx, holder), fmt.Sprintf("%s[%d]", id, i), node, deps, itemScope, opts)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
				return
			}
			outputs[i] = result.Output
			nodeIDs[i] = result.NodeID
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nodeResult{}, firstErr
	}
	aggregateID, err := plan.NodeID(plan.NodeIdentity{
		Task:         "taskpilot.forEach",
		Version:      "1",
		Inputs:       map[string]any{"node": id, "items": items, "itemNodeIds": nodeIDs},
		Predecessors: predecessorIDs(deps, scope),
	})
	if err != nil {
		return nodeResult{}, err
	}
	return nodeResult{NodeID: aggregateID, Output: outputs}, nil
}

// runLoopNode invokes the body task until continueWhen is false or
// maxIterations is reached.
func (e *Engine) runLoopNode(ctx context.Context, id string, node model.Node, deps []string, scope *scopeState, opts Options) (out nodeResult, err error) {
	ctx, span := e.tracer.Start(ctx, spanLabel(id, "loop "+node.Loop.BodyTask), trace.WithAttributes(
		attribute.String(telemetry.AttrSpanKind, telemetry.SpanKindLoop),
		attribute.String(telemetry.AttrRunID, opts.RunID),
		attribute.String(telemetry.AttrNodeName, id),
		attribute.String(telemetry.AttrTask, node.Loop.BodyTask),
	))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}()
	maxIterationsValue, err := resolveTemplate(node.Loop.MaxIterations, scope)
	if err != nil {
		return nodeResult{}, fmt.Errorf("node %s loop.maxIterations: %w", id, err)
	}
	maxIterations, ok := intValue(maxIterationsValue)
	if !ok || maxIterations < 1 {
		return nodeResult{}, fmt.Errorf("node %s loop.maxIterations must resolve to a positive integer", id)
	}
	state := map[string]any{}
	for k, v := range node.Loop.State {
		resolved, err := resolveTemplate(v, scope)
		if err != nil {
			return nodeResult{}, fmt.Errorf("node %s loop.state.%s: %w", id, k, err)
		}
		state[k] = resolved
	}
	var lastOutput any = map[string]any{}
	var lastNodeID string
	// Each pass resolves inputs with the current state and index, then may update
	// state from the body output.
	for i := 0; i < maxIterations; i++ {
		loopScope := scope.withState(state).withIndex(i)
		inputValue, err := resolveTemplate(node.Loop.Inputs, loopScope)
		if err != nil {
			return nodeResult{}, fmt.Errorf("node %s loop.inputs: %w", id, err)
		}
		input, ok := inputValue.(map[string]any)
		if !ok {
			return nodeResult{}, fmt.Errorf("node %s loop.inputs must resolve to an object", id)
		}
		input["iteration"] = i
		output, err := e.runTask(ctx, node.Loop.BodyTask, input, opts)
		if err != nil {
			return nodeResult{}, fmt.Errorf("node %s loop iteration %d: %w", id, i, err)
		}
		lastOutput = output
		lastNodeID, err = plan.NodeID(plan.NodeIdentity{
			Task:         node.Loop.BodyTask,
			Version:      e.taskVersion(node.Loop.BodyTask),
			Inputs:       map[string]any{"iteration": i, "input": input, "state": state},
			Predecessors: predecessorIDs(deps, scope),
		})
		if err != nil {
			return nodeResult{}, err
		}
		if outputMap, ok := output.(map[string]any); ok {
			if nextState, ok := outputMap["state"].(map[string]any); ok {
				state = nextState
			}
		}
		continueValue, err := resolveTemplate(node.Loop.ContinueWhen, loopScope.withBody(output))
		if err != nil {
			return nodeResult{}, fmt.Errorf("node %s loop.continueWhen: %w", id, err)
		}
		shouldContinue, ok := continueValue.(bool)
		if !ok {
			return nodeResult{}, fmt.Errorf("node %s loop.continueWhen must resolve to boolean", id)
		}
		if !shouldContinue {
			break
		}
	}
	if lastNodeID == "" {
		lastNodeID, err = plan.NodeID(plan.NodeIdentity{
			Task:         "taskpilot.loop",
			Version:      "1",
			Inputs:       map[string]any{"node": id, "state": state},
			Predecessors: predecessorIDs(deps, scope),
		})
		if err != nil {
			return nodeResult{}, err
		}
	}
	return nodeResult{NodeID: lastNodeID, Output: lastOutput}, nil
}

// runSingleNode resolves and validates inputs, computes the node identity, and
// either serves cache or executes the task.
func (e *Engine) runSingleNode(ctx context.Context, id string, node model.Node, deps []string, scope *scopeState, opts Options) (nodeResult, error) {
	baseName, itemIndex, isItem := splitForEachID(id)
	nodeAttrs := []attribute.KeyValue{
		attribute.String(telemetry.AttrSpanKind, telemetry.SpanKindNode),
		attribute.String(telemetry.AttrRunID, opts.RunID),
		attribute.String(telemetry.AttrNodeName, id),
		attribute.String(telemetry.AttrTask, node.Task),
	}
	if isItem {
		nodeAttrs = append(nodeAttrs, attribute.Int(telemetry.AttrForEachIndex, itemIndex))
	}
	ctx, span := e.tracer.Start(ctx, spanLabel(baseName, node.Task), trace.WithAttributes(nodeAttrs...))
	defer span.End()
	fail := func(stage string, err error) {
		span.SetAttributes(attribute.String(telemetry.AttrStage, stage))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}

	inputValue, err := resolveTemplate(node.Inputs, scope)
	if err != nil {
		err = fmt.Errorf("node %s inputs: %w", id, err)
		fail("input_resolution", err)
		return nodeResult{}, err
	}
	input, ok := inputValue.(map[string]any)
	if !ok {
		err := fmt.Errorf("node %s inputs must resolve to an object", id)
		fail("input_resolution", err)
		return nodeResult{}, err
	}
	if err := e.validator.Validate(e.inputSchemaFor(node.Task), input); err != nil {
		err = fmt.Errorf("node %s input schema: %w", id, err)
		fail("input_validation", err)
		return nodeResult{}, err
	}
	predecessors := predecessorIDs(deps, scope)
	version := node.Version
	if version == "" {
		version = e.taskVersion(node.Task)
	}
	identityInputs := e.runtime.IdentityInputs(node.Task, input)
	var externalDigest string
	memoize := true
	if !opts.DryRun {
		var derr error
		memoize, externalDigest, derr = e.runtime.CacheBehavior(node.Task, input)
		if derr != nil {
			fail("cache_behavior", derr)
			return nodeResult{}, derr
		}
	}
	runIDScoped := e.dependsOnRunID(node.Task, map[string]bool{}) || templateUsesRunID(node.Inputs)
	if runIDScoped {
		identityInputs = map[string]any{"runId": opts.RunID}
	}
	memoize = memoize && !runIDScoped
	nodeID, err := plan.NodeID(plan.NodeIdentity{Task: node.Task, Version: version, Inputs: identityInputs, External: externalDigest, Predecessors: predecessors})
	if err != nil {
		fail("node_identity", err)
		return nodeResult{}, err
	}
	span.SetAttributes(
		attribute.String(telemetry.AttrNodeID, nodeID),
		attribute.String(telemetry.AttrVersion, version),
	)
	if memoize {
		span.SetAttributes(attribute.String(telemetry.AttrCachePath, e.cache.EntryRef(nodeID).Path))
	}
	if !opts.DryRun && memoize {
		if cached, ok, err := e.cache.Read(nodeID); err != nil {
			fail("cache_read", err)
			return nodeResult{}, err
		} else if ok {
			decoded, derr := decodeCachedOutput(cached.Output)
			if derr != nil {
				fail("cache_decode", derr)
				return nodeResult{}, derr
			}
			e.cacheHits.Add(1)
			span.SetAttributes(attribute.String(telemetry.AttrCacheStatus, telemetry.CacheStatusHit))
			span.SetStatus(codes.Ok, "")
			return nodeResult{NodeID: nodeID, Output: decoded}, nil
		}
	}
	if !opts.DryRun && memoize {
		claim, cached, err := e.claimOrWaitForCache(ctx, span, id, nodeID, opts.RunID)
		if err != nil {
			return nodeResult{}, err
		}
		if cached != nil {
			decoded, derr := decodeCachedOutput(cached.Output)
			if derr != nil {
				fail("cache_decode", derr)
				return nodeResult{}, derr
			}
			e.cacheHits.Add(1)
			span.SetAttributes(attribute.String(telemetry.AttrCacheStatus, telemetry.CacheStatusHit))
			span.SetStatus(codes.Ok, "")
			return nodeResult{NodeID: nodeID, Output: decoded}, nil
		}
		// We hold the claim and will execute the node: a cacheable miss.
		e.cacheMisses.Add(1)
		defer func() {
			if rerr := claim.Release(); rerr != nil {
				span.RecordError(fmt.Errorf("node %s release claim: %w", id, rerr))
			}
		}()
	}

	var output any
	if _, ok := e.doc.Tasks[node.Task]; ok {
		output, err = e.runTask(ctx, node.Task, input, opts)
	} else {
		output, err = e.runtime.Execute(ctx, node.Task, input, builtin.Context{RunID: opts.RunID, NodeID: nodeID, DryRun: opts.DryRun})
	}
	if err != nil {
		fail("execution", err)
		return nodeResult{}, err
	}
	if node.OutputSchema != nil {
		if err := e.validator.Validate(node.OutputSchema, output); err != nil {
			err = fmt.Errorf("node %s output schema: %w", id, err)
			fail("output_validation", err)
			return nodeResult{}, err
		}
	}
	if opts.DryRun {
		span.SetAttributes(attribute.String(telemetry.AttrCacheStatus, telemetry.CacheStatusDryRun))
		span.SetStatus(codes.Ok, "")
		return nodeResult{NodeID: nodeID, Output: output}, nil
	}

	if !memoize {
		span.SetAttributes(attribute.String(telemetry.AttrCacheStatus, telemetry.CacheStatusNonCacheable))
		span.SetStatus(codes.Ok, "")
		return nodeResult{NodeID: nodeID, Output: output}, nil
	}
	entry := cache.Entry{NodeID: nodeID, Task: node.Task, Version: version, Predecessors: predecessors, CreatedAt: time.Now().UTC()}
	if entry.Output, err = json.Marshal(output); err != nil {
		fail("cache_encode", err)
		return nodeResult{}, err
	}
	// Commit is reached only on the success path: any execution error (including a
	// *retry.ResultError synthesized from a rejected/exhausted result) returns
	// above, so failed results are never cached.
	if err := e.cache.Commit(entry); err != nil {
		fail("cache_commit", err)
		return nodeResult{}, err
	}
	span.SetAttributes(attribute.String(telemetry.AttrCacheStatus, telemetry.CacheStatusMiss))
	span.SetStatus(codes.Ok, "")
	return nodeResult{NodeID: nodeID, Output: output}, nil
}

// claimOrWaitForCache acquires the cache claim, possibly reclaiming a stale
// claim, or waits for another worker to commit the entry.
func (e *Engine) claimOrWaitForCache(ctx context.Context, span trace.Span, nodeName, nodeID, runID string) (cache.Claim, *cache.Entry, error) {
	// Slots this execution path occupies (graph + forEach levels). While we
	// sleep-poll for a peer to commit this node we release all of them so blocked
	// ancestors can make progress, and re-acquire them before returning so the
	// caller's release defers stay balanced. Releasing inner-first and
	// re-acquiring outer-first gives every waiter the same global lock order,
	// which keeps re-acquisition deadlock-free.
	slots := heldConcurrencySlots(ctx)
	parked := false
	park := func() {
		if parked {
			return
		}
		for i := len(slots) - 1; i >= 0; i-- {
			slots[i].park()
		}
		parked = true
	}
	unpark := func() {
		if !parked {
			return
		}
		for i := 0; i < len(slots); i++ {
			slots[i].unpark()
		}
		parked = false
	}
	// Restore slots on every return path, including context cancellation.
	defer unpark()

	waitStarted := time.Time{}
	for {
		claim, err := e.cache.Claim(nodeID, runID)
		if err != nil {
			return cache.Claim{}, nil, err
		}
		if claim.Held() {
			if !waitStarted.IsZero() {
				span.AddEvent(telemetry.EventClaimAcquired, trace.WithAttributes(
					attribute.Int64("durationMs", durationMillis(waitStarted)),
				))
			}
			return claim, nil, nil
		}
		if cached, ok, err := e.cache.Read(nodeID); err != nil {
			return cache.Claim{}, nil, err
		} else if ok {
			return cache.Claim{}, &cached, nil
		}
		if waitStarted.IsZero() {
			waitStarted = time.Now()
			span.AddEvent(telemetry.EventClaimWait)
		}
		// Park: give up our concurrency slots while polling, so a worker that
		// holds the claim for this node can be scheduled and commit it. Holding
		// them here is what deadlocks a saturated forEach/graph pool.
		park()
		select {
		case <-ctx.Done():
			err := fmt.Errorf("node %s waiting for claim: %w", nodeName, ctx.Err())
			span.SetAttributes(attribute.String(telemetry.AttrStage, "cache_claim_wait"))
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return cache.Claim{}, nil, err
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// durationMillis returns the elapsed wait time in milliseconds.
func durationMillis(started time.Time) int64 {
	return time.Since(started).Milliseconds()
}

// predecessorIDs returns completed predecessor IDs in dependency order.
func predecessorIDs(deps []string, scope *scopeState) []string {
	predecessors := make([]string, 0, len(deps))
	for _, dep := range deps {
		predecessors = append(predecessors, scope.nodes[dep].NodeID)
	}
	return predecessors
}

// inputSchemaFor returns the input schema for a document task or builtin task.
func (e *Engine) inputSchemaFor(name string) any {
	if task, ok := e.doc.Tasks[name]; ok {
		return task.InputSchema
	}
	for _, spec := range builtin.SchemaRegistry().All() {
		if spec.Name == name {
			return spec.InputSchema
		}
	}
	return nil
}

// dependsOnRunID reports whether a task graph reads the run ID directly or
// through a nested task.
func (e *Engine) dependsOnRunID(name string, seen map[string]bool) bool {
	if name == builtin.RunIDTaskName {
		return true
	}
	if seen[name] {
		return false
	}
	seen[name] = true
	task, ok := e.doc.Tasks[name]
	if !ok || task.Graph == nil {
		return false
	}
	for _, node := range task.Graph.Nodes {
		if e.dependsOnRunID(node.Task, seen) || templateUsesRunID(node.Inputs) {
			return true
		}
		if node.ForEach != nil {
			for _, tf := range node.ForEach.RefTemplates() {
				if templateUsesRunID(tf.Value) {
					return true
				}
			}
		}
		if node.Loop != nil {
			if e.dependsOnRunID(node.Loop.BodyTask, seen) {
				return true
			}
			for _, tf := range node.Loop.RefTemplates() {
				if templateUsesRunID(tf.Value) {
					return true
				}
			}
		}
	}
	return templateUsesRunID(task.Graph.Output)
}

// intValue coerces common integer-like values used for concurrency and
// iteration limits.
func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), n == float64(int(n))
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

// isRunIDAlias reports whether name is one of the engine's run ID aliases.
func isRunIDAlias(name string) bool {
	return name == "id" || name == "RunId"
}

// templateUsesRunID reports whether a template reads the run ID namespace.
func templateUsesRunID(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		if from, _ := t["$from"].(string); from == "run" {
			name, _ := t["name"].(string)
			return isRunIDAlias(name)
		}
		for _, child := range t {
			if templateUsesRunID(child) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if templateUsesRunID(child) {
				return true
			}
		}
	}
	return false
}

// taskVersion returns the effective version for a document task or builtin
// task.
func (e *Engine) taskVersion(name string) string {
	if task, ok := e.doc.Tasks[name]; ok {
		if task.Version != "" {
			return task.Version
		}
		h, err := canonical.Hash("taskpilot.plan.v1", task)
		if err == nil {
			return h
		}
		return "1"
	}
	for _, spec := range builtin.SchemaRegistry().All() {
		if spec.Name == name {
			return spec.Version
		}
	}
	return "1"
}

// nodeResult carries the node ID and output for a completed or cached node.
type nodeResult struct {
	NodeID string
	Output any
}

// decodeCachedOutput decodes a stored JSON payload into the in-memory output
// value.
func decodeCachedOutput(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// scopeState carries the template namespaces visible at the current execution
// point.
type scopeState struct {
	input     map[string]any
	constants map[string]any
	nodes     map[string]nodeResult
	runID     string
	item      any
	index     *int
	state     map[string]any
	body      any
}

// clone makes a shallow copy of the current scope for child evaluation.
func (s *scopeState) clone() *scopeState {
	c := *s
	return &c
}

// withItem returns a child scope whose item and index shadow outer values.
func (s *scopeState) withItem(item any, index int) *scopeState {
	c := s.clone()
	c.item = item
	c.index = &index
	return c
}

// withIndex returns a child scope whose index shadows the outer value.
func (s *scopeState) withIndex(index int) *scopeState {
	c := s.clone()
	c.index = &index
	return c
}

// withState returns a child scope whose state shadows the outer value.
func (s *scopeState) withState(state map[string]any) *scopeState {
	c := s.clone()
	c.state = state
	return c
}

// withBody returns a child scope whose body output shadows the outer value.
func (s *scopeState) withBody(body any) *scopeState {
	c := s.clone()
	c.body = body
	return c
}

// resolveTemplate resolves a template against the active execution scope.
func resolveTemplate(template any, scope *scopeState) (any, error) {
	switch t := template.(type) {
	case nil:
		return map[string]any{}, nil
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			r, err := resolveTemplate(v, scope)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		if lit, ok := t["$literal"]; ok {
			return lit, nil
		}
		if from, ok := t["$from"].(string); ok {
			return resolveRef(from, t, scope)
		}
		out := map[string]any{}
		for k, v := range t {
			r, err := resolveTemplate(v, scope)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	default:
		return t, nil
	}
}

// resolveRef reads a value from a scope namespace and applies any path
// projection.
func resolveRef(from string, ref map[string]any, scope *scopeState) (any, error) {
	var value any
	switch from {
	case "input":
		name, _ := ref["name"].(string)
		value = scope.input[name]
	case "constant":
		name, _ := ref["name"].(string)
		value = scope.constants[name]
	case "node":
		node, _ := ref["node"].(string)
		value = scope.nodes[node].Output
	case "item":
		value = scope.item
	case "index":
		if scope.index != nil {
			value = *scope.index
		}
	case "state":
		name, _ := ref["name"].(string)
		value = scope.state[name]
	case "body":
		value = scope.body
	case "run":
		name, _ := ref["name"].(string)
		if isRunIDAlias(name) {
			value = scope.runID
		}
	default:
		return nil, fmt.Errorf("unknown template namespace %q", from)
	}
	if path, ok := ref["path"].([]any); ok {
		for _, seg := range path {
			var found bool
			value, found = project(value, fmt.Sprint(seg))
			if !found {
				return nil, fmt.Errorf("path segment %q not found", seg)
			}
		}
	}
	return value, nil
}

// project looks up a named child in a map-backed object.
func project(value any, segment string) (any, bool) {
	if obj, ok := value.(map[string]any); ok {
		v, exists := obj[segment]
		return v, exists
	}
	return nil, false
}

// collectRefs returns node references found anywhere in a template value.
func collectRefs(v any) []string {
	set := map[string]bool{}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if from, _ := t["$from"].(string); from == "node" {
				if n, _ := t["node"].(string); n != "" {
					set[n] = true
				}
			}
			for _, v := range t {
				walk(v)
			}
		case []any:
			for _, v := range t {
				walk(v)
			}
		}
	}
	walk(v)
	var out []string
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
