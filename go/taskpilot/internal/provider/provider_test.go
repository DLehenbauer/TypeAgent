package provider

import (
	"context"
	"errors"
	"testing"
)

func TestSetGet(t *testing.T) {
	fake := &FakeProvider{ProviderName: NamePwsh}
	set := NewSet(fake)

	got, ok := set.Get(NamePwsh)
	if !ok || got != fake {
		t.Fatalf("Get(pwsh) = %v, %v", got, ok)
	}
	if _, ok := set.Get("missing"); ok {
		t.Fatal("missing provider should not be found")
	}

	var nilSet *Set
	if _, ok := nilSet.Get(NamePwsh); ok {
		t.Fatal("nil set should report not found")
	}
}

func TestDefaultRegistersKnownProviders(t *testing.T) {
	set := Default(DefaultLimits())
	for _, name := range []Name{NamePwsh, NameCopilot} {
		if _, ok := set.Get(name); !ok {
			t.Fatalf("default set missing %q", name)
		}
	}
}

func TestFakeProviderRecordsRequestsAndReturnsResult(t *testing.T) {
	fake := &FakeProvider{Respond: StaticResult("value")}
	out, err := fake.Submit(context.Background(), Request{Input: map[string]any{"a": 1}}).Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out != "value" {
		t.Fatalf("out = %v", out)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 || reqs[0].Input["a"] != 1 {
		t.Fatalf("requests = %#v", reqs)
	}
}

func TestFakeProviderRespondComputesResult(t *testing.T) {
	fake := &FakeProvider{
		Respond: func(req Request) (Result, error) { return req.Input["echo"], nil },
	}
	out, err := fake.Submit(context.Background(), Request{Input: map[string]any{"echo": "hi"}}).Await(context.Background())
	if err != nil || out != "hi" {
		t.Fatalf("out=%v err=%v", out, err)
	}
}

func TestFutureAwaitRespectsContextCancellation(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	fake := &FakeProvider{Respond: func(Request) (Result, error) {
		<-release // block until the test releases the goroutine
		return nil, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	future := fake.Submit(context.Background(), Request{})
	cancel()
	if _, err := future.Await(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
