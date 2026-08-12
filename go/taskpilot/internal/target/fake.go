package target

import (
	"context"
	"sync"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/script"
)

// FakeKind is the kind FakeBackend registers under.
const FakeKind Kind = "fake"

// FakeRun records one Run call.
type FakeRun struct {
	ID         string
	State      string
	Checkpoint string
	Script     string
}

// FakeBackend is an in-memory Backend for engine and task tests. It
// records every call and models the one behaviour tests care about: an ensure
// run snapshots the state it was told to commit, so a later run can observe
// that the state already exists.
type FakeBackend struct {
	// BaselineState is the state Acquire reports for a fresh context.
	BaselineState string
	// RunResult is returned from every Run call.
	RunResult script.Result
	// RefuseCommit makes checkpointing runs report that they did not snapshot, modelling
	// a convergence body that "succeeded" with a non-zero exit. A caller must not
	// advance a lease on such a run.
	RefuseCommit bool

	mu        sync.Mutex
	acquired  int
	runs      []FakeRun
	released  []string
	kept      []string
	snapshots map[string]bool
	sweeps    int
}

func (f *FakeBackend) Kind() Kind { return FakeKind }

func (f *FakeBackend) Acquire(context.Context, AcquireRequest) (Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired++
	state := f.BaselineState
	if state == "" {
		state = "baseline"
	}
	return Instance{ID: "fake-instance", BaselineState: state, MaterializedState: state}, nil
}

func (f *FakeBackend) Release(_ context.Context, id string, keep bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if keep {
		f.kept = append(f.kept, id)
		return nil
	}
	f.released = append(f.released, id)
	return nil
}

func (f *FakeBackend) Run(_ context.Context, req RunRequest) (RunOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, FakeRun{
		ID: req.ID, State: req.State, Checkpoint: req.Checkpoint, Script: req.Script.Script,
	})
	committed := false
	if req.Checkpoint != "" && !f.RefuseCommit {
		if f.snapshots == nil {
			f.snapshots = map[string]bool{}
		}
		f.snapshots[req.Checkpoint] = true
		committed = true
	}
	out := f.RunResult
	if out.Stdout == "" && out.Stderr == "" && out.ExitCode == 0 && out.Value == nil {
		out = script.Result{Stdout: "{}", ExitCode: 0, Value: map[string]any{}}
	}
	return RunOutcome{Result: out, Committed: committed}, nil
}

func (f *FakeBackend) Sweep(context.Context, time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps++
	return 1, nil
}

// Runs returns a copy of the recorded Run calls.
func (f *FakeBackend) Runs() []FakeRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeRun(nil), f.runs...)
}

// Released returns the ids torn down.
func (f *FakeBackend) Released() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.released...)
}

// Kept returns the ids released with keep set.
func (f *FakeBackend) Kept() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.kept...)
}

// Acquisitions reports how many times Acquire was called.
func (f *FakeBackend) Acquisitions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acquired
}

// Sweeps reports how many times Sweep was called.
func (f *FakeBackend) Sweeps() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sweeps
}

// HasSnapshot reports whether a checkpointing run committed the given state.
func (f *FakeBackend) HasSnapshot(state string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshots[state]
}
