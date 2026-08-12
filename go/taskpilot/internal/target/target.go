// Package target defines durable execution targets used by linear leases.
package target

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/script"
)

// Kind identifies a target backend.
type Kind string

// Instance is the physical target and baseline state returned by acquisition.
type Instance struct {
	ID                string
	BaselineState     string
	MaterializedState string
}

// AcquireRequest separates state-defining backend options from the operational
// policy used only if the run exits before lease.release.
type AcquireRequest struct {
	Options       map[string]any
	KeepOnFailure bool
}

// RunRequest asks a backend to materialize State, run Script, and optionally
// commit Checkpoint as the next state.
type RunRequest struct {
	ID         string
	State      string
	Checkpoint string
	Script     script.Request
}

// RunOutcome reports the script result and whether Checkpoint was committed.
type RunOutcome struct {
	Result    script.Result
	Committed bool
}

// Backend owns one kind of durable execution target.
type Backend interface {
	Kind() Kind
	Acquire(ctx context.Context, req AcquireRequest) (Instance, error)
	Release(ctx context.Context, id string, keep bool) error
	Run(ctx context.Context, req RunRequest) (RunOutcome, error)
	Sweep(ctx context.Context, maxAge time.Duration) (int, error)
}

// Registry resolves target backends by kind.
type Registry struct {
	mu     sync.RWMutex
	byKind map[Kind]Backend
}

func NewRegistry(backends ...Backend) *Registry {
	r := &Registry{byKind: map[Kind]Backend{}}
	for _, backend := range backends {
		r.byKind[backend.Kind()] = backend
	}
	return r
}

func (r *Registry) Get(kind Kind) (Backend, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	backend, ok := r.byKind[kind]
	return backend, ok
}

func (r *Registry) Require(kind Kind) (Backend, error) {
	if backend, ok := r.Get(kind); ok {
		return backend, nil
	}
	return nil, fmt.Errorf("no execution target registered for kind %q (registered: %s)", kind, r.Kinds())
}

func (r *Registry) Kinds() string {
	if r == nil {
		return "none"
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.byKind))
	for kind := range r.byKind {
		names = append(names, string(kind))
	}
	if len(names) == 0 {
		return "none"
	}
	sort.Strings(names)
	return join(names)
}

func join(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += ", "
		}
		out += item
	}
	return out
}

func (r *Registry) SweepAll(ctx context.Context, maxAge time.Duration) (int, error) {
	if r == nil {
		return 0, nil
	}
	r.mu.RLock()
	backends := make([]Backend, 0, len(r.byKind))
	for _, backend := range r.byKind {
		backends = append(backends, backend)
	}
	r.mu.RUnlock()

	total := 0
	var firstErr error
	for _, backend := range backends {
		n, err := backend.Sweep(ctx, maxAge)
		total += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return total, firstErr
}

func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.mu.RLock()
	backends := make([]Backend, 0, len(r.byKind))
	for _, backend := range r.byKind {
		backends = append(backends, backend)
	}
	r.mu.RUnlock()
	for _, backend := range backends {
		if closer, ok := backend.(interface{ Close() }); ok {
			closer.Close()
		}
	}
}
