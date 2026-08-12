package verify_test

import (
	"strings"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/verify"
)

// leaseRegistry is a minimal registry exercising acquisition, threading, and
// terminal consumption.
type leaseRegistry struct{}

var leaseSpecs = map[string]model.TaskSpec{
	"lease.acquire": {
		Name:        "lease.acquire",
		Version:     "1",
		InputSchema: map[string]any{"type": "object"},
		EmitsLease:  true,
	},
	"pwsh.run": {
		Name:        "pwsh.run",
		Version:     "1",
		InputSchema: map[string]any{"type": "object"},
		LeaseInputs: []string{"runsOn"},
		EmitsLease:  true,
	},
	"lease.release": {
		Name:        "lease.release",
		Version:     "1",
		InputSchema: map[string]any{"type": "object"},
		LeaseInputs: []string{"lease"},
	},
	"lease.merge": {
		Name:        "lease.merge",
		Version:     "1",
		InputSchema: map[string]any{"type": "object"},
		LeaseInputs: []string{"left", "right"},
		EmitsLease:  true,
	},
}

func (leaseRegistry) Get(name string) (model.TaskSpec, bool) {
	s, ok := leaseSpecs[name]
	return s, ok
}

func (leaseRegistry) All() []model.TaskSpec {
	out := make([]model.TaskSpec, 0, len(leaseSpecs))
	for _, s := range leaseSpecs {
		out = append(out, s)
	}
	return out
}

// nodeRef binds a producer's lease output. The path matters: verification
// distinguishes a reference that carries a lease from one that merely reads the
// same node's ordinary result.
func nodeRef(id string) map[string]any {
	return map[string]any{"$from": "node", "node": id, "path": []any{model.LeaseOutputKey}}
}

// resultRef reads a node's ordinary result, carrying no lease.
func resultRef(id string) map[string]any {
	return map[string]any{"$from": "node", "node": id, "path": []any{"result"}}
}

func inputRef(name string) map[string]any {
	return map[string]any{"$from": "input", "name": name}
}

func leaseDoc(nodes map[string]model.Node) *model.Document {
	return &model.Document{
		Kind:    model.DocumentKind,
		Version: model.DocumentVersion,
		Entry:   "main",
		Tasks: map[string]model.TaskDef{
			"main": {
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Graph:        &model.Graph{Nodes: nodes, Output: map[string]any{"ok": true}},
			},
		},
	}
}

func leaseDocWithExtraTasks(nodes map[string]model.Node, extra map[string]model.TaskDef) *model.Document {
	doc := leaseDoc(nodes)
	for name, task := range extra {
		doc.Tasks[name] = task
	}
	return doc
}

func innerTaskUsingInputVM() model.TaskDef {
	return model.TaskDef{
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Graph: &model.Graph{
			Nodes: map[string]model.Node{
				"run": {Task: "pwsh.run", Inputs: map[string]any{"runsOn": inputRef("vm"), "script": inputRef("script")}},
			},
			Output: map[string]any{"ok": true},
		},
	}
}

func verifyErr(t *testing.T, doc *model.Document) string {
	t.Helper()
	_, err := verify.Document(doc, leaseRegistry{})
	if err == nil {
		return ""
	}
	return err.Error()
}

// The happy path: acquire, thread through two steps, release. This must verify
// cleanly, or the rules are too strict to express the dev loop.
func TestLeaseChainThreadedAndReleasedIsValid(t *testing.T) {
	doc := leaseDoc(map[string]model.Node{
		"vm":       {Task: "lease.acquire", Inputs: map[string]any{"kind": "hyperv"}},
		"config":   {Task: "pwsh.run", Inputs: map[string]any{"runsOn": nodeRef("vm"), "script": "a"}},
		"deploy":   {Task: "pwsh.run", Inputs: map[string]any{"runsOn": nodeRef("config"), "script": "b"}},
		"teardown": {Task: "lease.release", Inputs: map[string]any{"lease": nodeRef("deploy")}},
	})
	if msg := verifyErr(t, doc); msg != "" {
		t.Fatalf("valid lease chain rejected: %s", msg)
	}
}

// Two consumers of one lease would mean two nodes holding the same external
// context concurrently -- exactly what linearity exists to prevent.
func TestLeaseWithTwoConsumersIsRejected(t *testing.T) {
	doc := leaseDoc(map[string]model.Node{
		"vm":       {Task: "lease.acquire", Inputs: map[string]any{"kind": "hyperv"}},
		"a":        {Task: "pwsh.run", Inputs: map[string]any{"runsOn": nodeRef("vm"), "script": "a"}},
		"b":        {Task: "pwsh.run", Inputs: map[string]any{"runsOn": nodeRef("vm"), "script": "b"}},
		"teardown": {Task: "lease.release", Inputs: map[string]any{"lease": nodeRef("a")}},
	})
	msg := verifyErr(t, doc)
	if !strings.Contains(msg, "consumed exactly once") {
		t.Fatalf("expected an exactly-once violation, got: %s", msg)
	}
}

// A chain that is never released leaks the underlying machine, so the unclaimed
// final lease must fail verification rather than surface at runtime.
func TestUnreleasedLeaseChainIsRejected(t *testing.T) {
	doc := leaseDoc(map[string]model.Node{
		"vm":     {Task: "lease.acquire", Inputs: map[string]any{"kind": "hyperv"}},
		"config": {Task: "pwsh.run", Inputs: map[string]any{"runsOn": nodeRef("vm"), "script": "a"}},
	})
	msg := verifyErr(t, doc)
	if !strings.Contains(msg, "no node consumes") {
		t.Fatalf("expected an unconsumed-lease violation, got: %s", msg)
	}
	if !strings.Contains(msg, "config") {
		t.Fatalf("error should name the node holding the dangling lease, got: %s", msg)
	}
}

// An acquired lease that is never bound anywhere is the degenerate leak.
func TestAcquiredButUnusedLeaseIsRejected(t *testing.T) {
	doc := leaseDoc(map[string]model.Node{
		"vm": {Task: "lease.acquire", Inputs: map[string]any{"kind": "hyperv"}},
	})
	if msg := verifyErr(t, doc); !strings.Contains(msg, "no node consumes") {
		t.Fatalf("expected an unconsumed-lease violation, got: %s", msg)
	}
}

// A terminal task with nothing bound to it is a mis-wired teardown.
func TestTerminalTaskWithoutALeaseIsRejected(t *testing.T) {
	doc := leaseDoc(map[string]model.Node{
		"teardown": {Task: "lease.release", Inputs: map[string]any{}},
	})
	if msg := verifyErr(t, doc); !strings.Contains(msg, "ends a lease chain but no lease is bound") {
		t.Fatalf("expected a dangling-terminator violation, got: %s", msg)
	}
}

// Graphs that never touch a lease must be entirely unaffected by these rules.
func TestLeaseFreeGraphIsUnaffected(t *testing.T) {
	doc := leaseDoc(map[string]model.Node{
		"a": {Task: "pwsh.run", Inputs: map[string]any{"script": "a"}},
		"b": {Task: "pwsh.run", Inputs: map[string]any{"script": "b"}},
	})
	if msg := verifyErr(t, doc); msg != "" {
		t.Fatalf("lease-free graph rejected: %s", msg)
	}
}

// Reading a lease-producing node's ordinary result is not consumption, so it
// must neither count as a consumer nor be rejected.
func TestReadingAProducersResultIsNotConsumption(t *testing.T) {
	doc := leaseDoc(map[string]model.Node{
		"vm":       {Task: "lease.acquire", Inputs: map[string]any{"kind": "hyperv"}},
		"config":   {Task: "pwsh.run", Inputs: map[string]any{"runsOn": nodeRef("vm"), "script": "a"}},
		"report":   {Task: "pwsh.run", Inputs: map[string]any{"script": "b", "note": resultRef("config")}},
		"teardown": {Task: "lease.release", Inputs: map[string]any{"lease": nodeRef("config")}},
	})
	if msg := verifyErr(t, doc); msg != "" {
		t.Fatalf("reading a producer's result was treated as consuming its lease: %s", msg)
	}
}

func TestLeaseInLoopInputsIsRejected(t *testing.T) {
	doc := leaseDocWithExtraTasks(map[string]model.Node{
		"vm": {Task: "lease.acquire", Inputs: map[string]any{"kind": "hyperv"}},
		"work": {
			Loop: &model.LoopSpec{
				BodyTask:      "inner",
				MaxIterations: 1,
				Inputs:        map[string]any{"vm": nodeRef("vm"), "script": "a"},
				ContinueWhen:  false,
			},
		},
		"rel": {Task: "lease.release", Inputs: map[string]any{"lease": nodeRef("vm")}},
	}, map[string]model.TaskDef{"inner": innerTaskUsingInputVM()})
	msg := verifyErr(t, doc)
	if !strings.Contains(msg, "loop.inputs") || !strings.Contains(msg, "loop node may not consume a lease") {
		t.Fatalf("expected a loop input lease violation, got: %s", msg)
	}
}

func TestLeaseInLoopStateIsRejected(t *testing.T) {
	doc := leaseDocWithExtraTasks(map[string]model.Node{
		"vm": {Task: "lease.acquire", Inputs: map[string]any{"kind": "hyperv"}},
		"work": {
			Loop: &model.LoopSpec{
				BodyTask:      "inner",
				MaxIterations: 1,
				State:         map[string]any{"vm": nodeRef("vm")},
				Inputs:        map[string]any{"script": "a"},
				ContinueWhen:  false,
			},
		},
	}, map[string]model.TaskDef{"inner": innerTaskUsingInputVM()})
	msg := verifyErr(t, doc)
	if !strings.Contains(msg, "loop.state") || !strings.Contains(msg, "loop node may not consume a lease") {
		t.Fatalf("expected a loop state lease violation, got: %s", msg)
	}
}

func TestLeaseFreeLoopAndForEachAreAccepted(t *testing.T) {
	doc := leaseDocWithExtraTasks(map[string]model.Node{
		"loop": {
			Loop: &model.LoopSpec{
				BodyTask:      "inner",
				MaxIterations: 2,
				State:         map[string]any{"vm": "local"},
				Inputs:        map[string]any{"vm": "local", "script": "loop"},
				ContinueWhen:  false,
			},
		},
		"fan": {
			Task:    "pwsh.run",
			Inputs:  map[string]any{"script": "fan"},
			ForEach: &model.ForEachSpec{Items: []any{1, 2, 3}, MaxConcurrency: 2},
		},
	}, map[string]model.TaskDef{"inner": innerTaskUsingInputVM()})
	if msg := verifyErr(t, doc); msg != "" {
		t.Fatalf("lease-free loop/forEach graph rejected: %s", msg)
	}
}

// A forEach body runs many times concurrently, so one static edge would stand
// for N concurrent users of a single context.
func TestLeaseOnForEachNodeIsRejected(t *testing.T) {
	doc := leaseDoc(map[string]model.Node{
		"vm": {Task: "lease.acquire", Inputs: map[string]any{"kind": "hyperv"}},
		"fan": {
			Task:    "pwsh.run",
			Inputs:  map[string]any{"runsOn": nodeRef("vm"), "script": "a"},
			ForEach: &model.ForEachSpec{Items: []any{1, 2, 3}},
		},
		"teardown": {Task: "lease.release", Inputs: map[string]any{"lease": nodeRef("fan")}},
	})
	if msg := verifyErr(t, doc); !strings.Contains(msg, "may not acquire or consume a lease") {
		t.Fatalf("expected a forEach lease violation, got: %s", msg)
	}
}

// A lease bound into a task the registry cannot describe would be consumed
// invisibly, letting a second real consumer still pass the exactly-once check.
func TestLeaseBoundToAnUndeclaredInputIsRejected(t *testing.T) {
	doc := leaseDoc(map[string]model.Node{
		"vm":       {Task: "lease.acquire", Inputs: map[string]any{"kind": "hyperv"}},
		"sneaky":   {Task: "pwsh.run", Inputs: map[string]any{"script": "a", "smuggled": nodeRef("vm")}},
		"teardown": {Task: "lease.release", Inputs: map[string]any{"lease": nodeRef("vm")}},
	})
	if msg := verifyErr(t, doc); !strings.Contains(msg, "not a lease input") {
		t.Fatalf("expected an undeclared-lease-input violation, got: %s", msg)
	}
}

func TestWholeNodeReferenceToLeaseEmitterIsRejected(t *testing.T) {
	doc := leaseDoc(map[string]model.Node{
		"vm":       {Task: "lease.acquire", Inputs: map[string]any{"kind": "hyperv"}},
		"sneaky":   {Task: "pwsh.run", Inputs: map[string]any{"script": "a", "data": map[string]any{"$from": "node", "node": "vm"}}},
		"teardown": {Task: "lease.release", Inputs: map[string]any{"lease": nodeRef("vm")}},
	})
	if msg := verifyErr(t, doc); !strings.Contains(msg, "without selecting a path") {
		t.Fatalf("expected whole-node lease reference violation, got: %s", msg)
	}
}

// A node emits at most one successor, so it cannot thread two leases.
func TestNodeConsumingTwoLeasesIsRejected(t *testing.T) {
	doc := leaseDoc(map[string]model.Node{
		"vmA":  {Task: "lease.acquire", Inputs: map[string]any{"kind": "hyperv"}},
		"vmB":  {Task: "lease.acquire", Inputs: map[string]any{"kind": "hyperv"}},
		"copy": {Task: "lease.merge", Inputs: map[string]any{"left": nodeRef("vmA"), "right": nodeRef("vmB")}},
		"teardown": {
			Task:   "lease.release",
			Inputs: map[string]any{"lease": nodeRef("copy")},
		},
	})
	if msg := verifyErr(t, doc); !strings.Contains(msg, "can emit only one successor") {
		t.Fatalf("expected a two-lease violation, got: %s", msg)
	}
}
