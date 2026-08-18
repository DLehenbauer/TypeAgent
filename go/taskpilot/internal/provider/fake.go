package provider

import (
	"context"
	"sync"
)

// nameFake is the default provider name for a FakeProvider with no ProviderName
// set. It is unexported because the fake provider is test-only wiring, not part
// of the production provider roster (NamePwsh, NameCopilot).
const nameFake Name = "fake"

// FakeProvider records submitted requests and returns scripted results. It is
// safe for concurrent use and performs no side effects, making it suitable for
// engine and task tests.
type FakeProvider struct {
	// ProviderName is the name this provider registers under. Defaults to nameFake.
	ProviderName Name

	// Respond computes the result for each request. When nil, Submit yields a
	// nil result and no error. Use StaticResult to return a fixed value.
	Respond func(Request) (Result, error)

	// Limit bounds how many Respond calls run concurrently, using the same
	// throttle production providers use. Zero or less runs unbounded.
	Limit int

	once     sync.Once
	tr       *throttle
	mu       sync.Mutex
	requests []Request
}

// StaticResult builds a Respond function that returns the same value and no
// error for every request.
func StaticResult(v Result) func(Request) (Result, error) {
	return func(Request) (Result, error) { return v, nil }
}

// Name reports the provider's name, returning nameFake when ProviderName is unset.
func (f *FakeProvider) Name() Name {
	if f.ProviderName == "" {
		return nameFake
	}
	return f.ProviderName
}

// throttle builds the shared throttle once so every Submit call shares the same limit.
func (f *FakeProvider) throttle() *throttle {
	f.once.Do(func() {
		f.tr = newThrottle(f.Limit)
	})
	return f.tr
}

// Submit records a defensive copy of req for later inspection via Requests and
// returns a Future. The result is produced by Respond when set; otherwise the
// Future resolves to a nil result and no error.
func (f *FakeProvider) Submit(ctx context.Context, req Request) Future {
	if req.Input != nil {
		input := make(Input, len(req.Input))
		for k, v := range req.Input {
			input[k] = v
		}
		req.Input = input
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	respond := f.Respond
	f.mu.Unlock()

	return f.throttle().dispatch(ctx, func(context.Context) (Result, error) {
		if respond != nil {
			return respond(req)
		}
		return nil, nil
	})
}

// Requests returns a copy of the requests submitted so far.
func (f *FakeProvider) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Request, len(f.requests))
	for i, req := range f.requests {
		if req.Input != nil {
			input := make(Input, len(req.Input))
			for k, v := range req.Input {
				input[k] = v
			}
			req.Input = input
		}
		out[i] = req
	}
	return out
}
