package engine

import (
	"context"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/builtin"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/cache"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/verify"
)

func leaseRef(node string) map[string]any {
	return map[string]any{"$from": "node", "node": node, "path": []any{builtin.LeaseOutput}}
}

// devLoopDoc mirrors the shape of the EPP workflow: acquire a context, converge
// two checkpointing layers, run a transient step, then release.
func devLoopDoc() *model.Document {
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
						"vm": {
							Task:   builtin.LeaseAcquireTaskName,
							Inputs: map[string]any{builtin.LeaseKindInput: string(target.FakeKind)},
						},
						"config": {
							Task: "pwsh.run",
							Inputs: map[string]any{
								builtin.PwshRunsOnInput: leaseRef("vm"),
								"script":                "Apply-Config",
								"checkpoint":            true,
							},
						},
						"deploy": {
							Task: "pwsh.run",
							Inputs: map[string]any{
								builtin.PwshRunsOnInput: leaseRef("config"),
								"script":                "Apply-Deploy",
								"checkpoint":            true,
							},
						},
						"tests": {
							Task: "pwsh.run",
							Inputs: map[string]any{
								builtin.PwshRunsOnInput: leaseRef("deploy"),
								"script":                "Invoke-Tests",
							},
						},
						"teardown": {
							Task:   builtin.LeaseReleaseTaskName,
							Inputs: map[string]any{builtin.LeaseInput: leaseRef("tests")},
						},
					},
					Output: map[string]any{"done": true},
				},
			},
		},
	}
}

func runDevLoop(t *testing.T, doc *model.Document, fake *target.FakeBackend, store *cache.Store, runID string) *Engine {
	t.Helper()
	rt := builtin.RuntimeRegistry()
	rt.SetServices(&builtin.Services{Providers: provider.NewSet(), Targets: target.NewRegistry(fake)})
	eng := New(doc, rt, store, nil)
	if _, err := eng.Run(context.Background(), Options{RunID: runID, Input: map[string]any{}, MaxParallel: 4}); err != nil {
		t.Fatalf("%s: %v", runID, err)
	}
	return eng
}

// The dev-loop shape must pass verification: leases threaded one-to-one and the
// chain terminated by a release.
func TestDevLoopGraphVerifies(t *testing.T) {
	if _, err := verify.Document(devLoopDoc(), builtin.SchemaRegistry()); err != nil {
		t.Fatalf("dev-loop graph failed verification: %v", err)
	}
}

// End to end: the lease threads through every node, checkpointing nodes snapshot the
// state named by their own node ID, the transient node does not advance state,
// and the chain is released.
func TestLeaseChainExecutesEndToEnd(t *testing.T) {
	fake := &target.FakeBackend{}
	runDevLoop(t, devLoopDoc(), fake, newTestStore(t, t.TempDir()), "run-1")

	if got := fake.Acquisitions(); got != 1 {
		t.Fatalf("acquired %d contexts, want 1", got)
	}
	execs := fake.Runs()
	if len(execs) != 3 {
		t.Fatalf("ran %d bodies, want 3", len(execs))
	}
	byScript := map[string]target.FakeRun{}
	for _, e := range execs {
		byScript[e.Script] = e
	}

	// Each checkpointing node commits under a non-empty state name.
	for _, script := range []string{"Apply-Config", "Apply-Deploy"} {
		e := byScript[script]
		if e.Checkpoint == "" {
			t.Fatalf("%s checkpointing run carried no state to commit", script)
		}
		if !fake.HasSnapshot(e.Checkpoint) {
			t.Fatalf("%s did not snapshot state %q", script, e.Checkpoint)
		}
	}

	// deploy must run on the state config established, proving the lease
	// advanced rather than resetting to the baseline.
	if byScript["Apply-Deploy"].State != byScript["Apply-Config"].Checkpoint {
		t.Fatalf("deploy ran at state %q, want config's committed state %q",
			byScript["Apply-Deploy"].State, byScript["Apply-Config"].Checkpoint)
	}

	// The transient node must run at deploy's state and must not claim a state
	// of its own -- nothing snapshotted it, so claiming one would be unrecoverable.
	tests := byScript["Invoke-Tests"]
	if tests.Checkpoint != "" {
		t.Fatalf("transient run claimed state %q; only a checkpointing run may advance state", tests.Checkpoint)
	}
	if tests.State != byScript["Apply-Deploy"].Checkpoint {
		t.Fatalf("transient run ran at state %q, want deploy's committed state %q",
			tests.State, byScript["Apply-Deploy"].Checkpoint)
	}

	if released := fake.Released(); len(released) != 1 {
		t.Fatalf("released %v, want exactly one context torn down", released)
	}
}

// A second run re-executes the checkpointing nodes and asks
// for exactly the same state names, which is what lets the provider recognize
// the states it already holds and skip the real work.
func TestLeaseStateNamesAreStableAcrossRuns(t *testing.T) {
	doc := devLoopDoc()
	store := newTestStore(t, t.TempDir())

	first := &target.FakeBackend{}
	runDevLoop(t, doc, first, store, "run-1")
	second := &target.FakeBackend{}
	runDevLoop(t, doc, second, store, "run-2")

	commits := func(f *target.FakeBackend) map[string]string {
		out := map[string]string{}
		for _, e := range f.Runs() {
			if e.Checkpoint != "" {
				out[e.Script] = e.Checkpoint
			}
		}
		return out
	}
	a, b := commits(first), commits(second)
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("expected two checkpointing runs each time, got %d and %d", len(a), len(b))
	}
	for script, state := range a {
		if b[script] != state {
			t.Fatalf("%s committed %q on the first run and %q on the second; state names must be stable",
				script, state, b[script])
		}
	}
}

func TestKeepOnFailureDoesNotInvalidateLeaseStateNames(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	firstDoc := devLoopDoc()
	first := &target.FakeBackend{}
	runDevLoop(t, firstDoc, first, store, "run-keep")

	secondDoc := devLoopDoc()
	vm := secondDoc.Tasks["main"].Graph.Nodes["vm"]
	vm.Inputs[builtin.LeaseKeepOnFailureInput] = false
	secondDoc.Tasks["main"].Graph.Nodes["vm"] = vm
	second := &target.FakeBackend{}
	runDevLoop(t, secondDoc, second, store, "run-clean")

	commits := func(backend *target.FakeBackend) map[string]string {
		out := map[string]string{}
		for _, run := range backend.Runs() {
			if run.Checkpoint != "" {
				out[run.Script] = run.Checkpoint
			}
		}
		return out
	}
	a, b := commits(first), commits(second)
	for script, state := range a {
		if b[script] != state {
			t.Fatalf("%s state changed with keepOnFailure: %q -> %q", script, state, b[script])
		}
	}
}

func TestTargetIdentityIgnoresCredentialsButIncludesWorkspace(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	firstDoc := devLoopDoc()
	firstVM := firstDoc.Tasks["main"].Graph.Nodes["vm"]
	firstVM.Inputs[builtin.LeaseOptionsInput] = map[string]any{
		"baseImage": "base.vhdx",
		"guest":     map[string]any{"username": "first", "passwordEnv": "FIRST_PASSWORD"},
		"workspace": map[string]any{
			"uncPath": `\\host\share`, "drive": "Z:", "username": "worker",
			"passwordEnv": "FIRST_WORKSPACE_PASSWORD",
		},
	}
	firstDoc.Tasks["main"].Graph.Nodes["vm"] = firstVM
	first := &target.FakeBackend{}
	runDevLoop(t, firstDoc, first, store, "run-first")

	secondDoc := devLoopDoc()
	secondVM := secondDoc.Tasks["main"].Graph.Nodes["vm"]
	secondVM.Inputs[builtin.LeaseOptionsInput] = map[string]any{
		"baseImage": "base.vhdx",
		"guest":     map[string]any{"username": "second", "passwordEnv": "SECOND_PASSWORD"},
		"workspace": map[string]any{
			"uncPath": `\\host\share`, "drive": "Z:", "username": "worker",
			"passwordEnv": "SECOND_WORKSPACE_PASSWORD",
		},
	}
	secondDoc.Tasks["main"].Graph.Nodes["vm"] = secondVM
	second := &target.FakeBackend{}
	runDevLoop(t, secondDoc, second, store, "run-second")

	firstRuns, secondRuns := first.Runs(), second.Runs()
	for i := range firstRuns {
		if firstRuns[i].Checkpoint != secondRuns[i].Checkpoint {
			t.Fatalf("checkpoint changed with credential-only options: %q -> %q",
				firstRuns[i].Checkpoint, secondRuns[i].Checkpoint)
		}
	}

	thirdDoc := devLoopDoc()
	thirdVM := thirdDoc.Tasks["main"].Graph.Nodes["vm"]
	thirdVM.Inputs[builtin.LeaseOptionsInput] = map[string]any{
		"baseImage": "base.vhdx",
		"guest":     map[string]any{"username": "second", "passwordEnv": "SECOND_PASSWORD"},
		"workspace": map[string]any{
			"uncPath": `\\host\other`, "drive": "Y:", "username": "worker",
			"passwordEnv": "SECOND_WORKSPACE_PASSWORD",
		},
	}
	thirdDoc.Tasks["main"].Graph.Nodes["vm"] = thirdVM
	third := &target.FakeBackend{}
	runDevLoop(t, thirdDoc, third, store, "run-third")

	thirdRuns := third.Runs()
	changed := false
	for i := range firstRuns {
		if firstRuns[i].Checkpoint != thirdRuns[i].Checkpoint {
			changed = true
			break
		}
	}
	if !changed {
		t.Fatal("workspace location change did not invalidate checkpoint identity")
	}
}

// keep leaves the context up for inspection while still satisfying the
// terminal-release rule, so the rule is not too rigid to express that intent.
func TestReleaseWithKeepDoesNotTearDown(t *testing.T) {
	doc := devLoopDoc()
	teardown := doc.Tasks["main"].Graph.Nodes["teardown"]
	teardown.Inputs[builtin.LeaseKeepInput] = true
	doc.Tasks["main"].Graph.Nodes["teardown"] = teardown

	fake := &target.FakeBackend{}
	runDevLoop(t, doc, fake, newTestStore(t, t.TempDir()), "run-keep")

	if got := fake.Released(); len(got) != 0 {
		t.Fatalf("context was torn down despite keep: %v", got)
	}
	if got := fake.Kept(); len(got) != 1 {
		t.Fatalf("kept %v, want exactly one retained context", got)
	}
}

// The lease lifecycle builtins must never be memoized. Their inputs are stable,
// so a cached second run would hand back the first run's lease without
// acquiring anything -- and would skip teardown entirely, leaking the machine.
func TestLeaseLifecycleIsNeverServedFromCache(t *testing.T) {
	doc := devLoopDoc()
	store := newTestStore(t, t.TempDir())

	first := &target.FakeBackend{}
	runDevLoop(t, doc, first, store, "run-1")
	second := &target.FakeBackend{}
	runDevLoop(t, doc, second, store, "run-2")

	if got := second.Acquisitions(); got != 1 {
		t.Fatalf("second run acquired %d contexts, want 1; lease.acquire was served from cache", got)
	}
	if got := second.Released(); len(got) != 1 {
		t.Fatalf("second run released %v, want one teardown; lease.release was served from cache", got)
	}
}

// A convergence body can "succeed" without converging -- a non-zero exit is an
// ordinary result. The lease must not advance to a state that was never
// snapshotted, or the next restore would fail against a checkpoint that does
// not exist.
func TestLeaseDoesNotAdvanceWhenNothingWasCommitted(t *testing.T) {
	fake := &target.FakeBackend{RefuseCommit: true}
	runDevLoop(t, devLoopDoc(), fake, newTestStore(t, t.TempDir()), "run-nocommit")

	for _, e := range fake.Runs() {
		if e.State != "baseline" {
			t.Fatalf("%s ran at state %q; with nothing committed every node must stay at the baseline",
				e.Script, e.State)
		}
	}
}
