package target_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
)

func TestRegistryRequiresRegisteredKind(t *testing.T) {
	registry := target.NewRegistry(&target.FakeBackend{})
	if _, err := registry.Require(target.FakeKind); err != nil {
		t.Fatal(err)
	}
	_, err := registry.Require("missing")
	if err == nil || !strings.Contains(err.Error(), string(target.FakeKind)) {
		t.Fatalf("error = %v, want registered kinds", err)
	}
}

func TestSweepAllAsksEveryBackend(t *testing.T) {
	fake := &target.FakeBackend{}
	n, err := target.NewRegistry(fake).SweepAll(context.Background(), time.Hour)
	if err != nil || n != 1 || fake.Sweeps() != 1 {
		t.Fatalf("SweepAll = %d, %v; sweeps=%d", n, err, fake.Sweeps())
	}
}

func TestNilRegistryIsSafe(t *testing.T) {
	var registry *target.Registry
	if _, ok := registry.Get("anything"); ok {
		t.Fatal("nil registry returned a backend")
	}
	if n, err := registry.SweepAll(context.Background(), time.Hour); err != nil || n != 0 {
		t.Fatalf("SweepAll = %d, %v", n, err)
	}
	if registry.Kinds() != "none" {
		t.Fatalf("Kinds = %q", registry.Kinds())
	}
}
