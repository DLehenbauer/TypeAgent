// Package provider centralizes side-effecting actions (PowerShell, Copilot, ...)
// behind a Submit/Await seam. Tasks submit work items and await their results;
// providers own how that work is dispatched, including transient-error retry,
// since only a provider knows which of its failures are safe to re-issue.
//
// Each provider bounds its own concurrency via a throttle: Submit is
// non-blocking and at most N items run at once, with excess work queued. This
// makes the provider the natural throttle for the graph's implicit fan-out
// parallelism. Rate limiting can be layered into the same dispatch step later.
package provider

import (
	"context"

	"golang.org/x/sync/semaphore"
)

// Input is a provider request payload: the decoded task input a provider
// dispatches. Its keys and value shapes are each provider's own I/O contract
// (e.g. the Pwsh* input keys or the copilot* keys), so it is a map of
// JSON-shaped values rather than a fixed struct -- one provider seam serves
// many task schemas. It is the request half of the provider boundary contract.
type Input map[string]any

// Result is a provider result payload: the JSON-shaped value a provider
// produces (e.g. the pwsh result envelope or a Copilot reply). It is the result
// half of the provider boundary contract; each provider documents its own
// concrete shape.
type Result any

// Request is a unit of work submitted to a provider.
type Request struct {
	Input Input
}

// Future represents an in-flight or completed work item.
type Future interface {
	// Await blocks until the work item completes or ctx is cancelled.
	Await(ctx context.Context) (Result, error)
}

// Name identifies a provider category. It is a distinct type rather than a bare
// string so provider constants, Name(), registry keys, and lookups share one
// typed contract and an arbitrary string can't stand in for a provider name.
type Name string

// Provider names identify each provider category. They are the single source
// of truth for both a provider's Name() and the key callers use to look it up
// in a Set.
const (
	NamePwsh    Name = "pwsh"
	NameCopilot Name = "copilot"
)

// Provider dispatches work items of a single category (e.g. "pwsh", "copilot").
type Provider interface {
	Name() Name
	Submit(ctx context.Context, req Request) Future
}

type result struct {
	value Result
	err   error
}

type chanFuture struct {
	ch <-chan result
}

func (f chanFuture) Await(ctx context.Context) (Result, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-f.ch:
		return r.value, r.err
	}
}

// throttle bounds how many work items a provider runs concurrently. It is the
// single seam where dispatch is gated: Submit stays non-blocking (it enqueues
// and returns a Future immediately) while at most N items execute at once.
// Excess submissions wait in the semaphore queue, so backpressure surfaces as
// wait, never as rejected work. A nil throttle (or a non-positive limit) runs
// every item immediately, preserving the original unbounded behavior.
//
// Concurrency is the only dimension enforced today. A rate limiter (e.g.
// golang.org/x/time/rate) would plug into the same acquire step: wait for a
// token, then a slot, before running fn.
type throttle struct {
	sem *semaphore.Weighted
}

// newThrottle returns a throttle bounded to limit concurrent items. A limit of
// zero or less yields an unbounded throttle.
func newThrottle(limit int) *throttle {
	if limit <= 0 {
		return &throttle{}
	}
	return &throttle{sem: semaphore.NewWeighted(int64(limit))}
}

// dispatch runs fn in its own goroutine and returns a Future for the result.
// The slot is acquired inside the goroutine so Submit never blocks. An item
// cancelled while still queued returns ctx.Err() without ever running fn, so a
// cancelled submission has no side effect. This is the single place where work
// is dispatched, so a future pass can layer in rate limiting here without
// touching providers or callers.
func (t *throttle) dispatch(ctx context.Context, fn func(context.Context) (Result, error)) Future {
	ch := make(chan result, 1)
	go func() {
		if t.sem != nil {
			if err := t.sem.Acquire(ctx, 1); err != nil {
				ch <- result{err: err}
				return
			}
			defer t.sem.Release(1)
		}
		value, err := fn(ctx)
		ch <- result{value: value, err: err}
	}()
	return chanFuture{ch: ch}
}

// Set is a name-keyed collection of providers injected into task execution.
type Set struct {
	byName map[Name]Provider
}

// NewSet builds a Set, keying each provider by its Name().
func NewSet(providers ...Provider) *Set {
	s := &Set{byName: make(map[Name]Provider, len(providers))}
	for _, p := range providers {
		s.byName[p.Name()] = p
	}
	return s
}

// Get returns the provider registered under name.
func (s *Set) Get(name Name) (Provider, bool) {
	if s == nil {
		return nil, false
	}
	p, ok := s.byName[name]
	return p, ok
}

// Close releases any provider in the set that owns process-level resources
// (e.g. the Copilot provider's shared CLI server). Providers without such
// resources are skipped. Safe to call once after a run completes.
func (s *Set) Close() {
	if s == nil {
		return
	}
	for _, p := range s.byName {
		if c, ok := p.(interface{ Close() }); ok {
			c.Close()
		}
	}
}

// Limits maps a provider name to its concurrency cap for Default. A missing or
// non-positive value leaves the corresponding provider unbounded.
type Limits map[Name]int

// Default per-provider concurrency caps. Copilot sessions are heavy and often
// server-side rate limited; pwsh runs local processes and can tolerate a wider
// fan-out. Copilot concurrency is calculated based on available system RAM
// (approximately 256MB per session for conservative headroom).
const (
	DefaultPwshParallel    = 8
	bytesPerCopilotSession = 256 * 1024 * 1024 // 256MB per session
)

// DefaultCopilotParallel computes the default concurrency for copilot.invoke
// based on available system RAM (approximately 256MB per session for
// conservative headroom). It queries total physical system memory and divides
// by bytesPerCopilotSession, with a floor of 1. getTotalSystemMemory applies a
// shared undetermined-memory fallback over the per-OS probeTotalSystemMemory.
func DefaultCopilotParallel() int {
	totalMemory := getTotalSystemMemory()
	concurrency := int(totalMemory / bytesPerCopilotSession / 4)
	if concurrency < 1 {
		return 1
	}
	return concurrency
}

// providerSpec is the single source of truth for one built-in provider: its
// name, default concurrency cap, and constructor wiring. Adding a provider or
// changing its default is one catalog entry rather than edits scattered across
// DefaultLimits and Default.
type providerSpec struct {
	name         Name
	defaultLimit int
	build        func(limit int) Provider
}

// catalog enumerates the built-in providers wired by Default. It is the one
// place that pairs each provider's name, default cap, and constructor. It is a
// function rather than a package-level var so each call builds a fresh,
// caller-owned slice with no shared mutable state, and so Copilot's RAM-derived
// default cap is computed at call time rather than frozen at package init.
func catalog() []providerSpec {
	return []providerSpec{
		{name: NamePwsh, defaultLimit: DefaultPwshParallel, build: func(limit int) Provider { return NewPwshProvider(limit) }},
		{name: NameCopilot, defaultLimit: DefaultCopilotParallel(), build: func(limit int) Provider { return NewCopilotProvider(nil, limit) }},
	}
}

// DefaultLimits returns the built-in per-provider concurrency caps.
func DefaultLimits() Limits {
	specs := catalog()
	limits := make(Limits, len(specs))
	for _, spec := range specs {
		limits[spec.name] = spec.defaultLimit
	}
	return limits
}

// Default wires the real providers backed by live system integrations, applying
// the given concurrency limits. A provider absent from limits runs unbounded.
func Default(limits Limits) *Set {
	providers := make([]Provider, 0)
	for _, spec := range catalog() {
		if spec.build != nil {
			providers = append(providers, spec.build(limits[spec.name]))
		}
	}
	return NewSet(providers...)
}
