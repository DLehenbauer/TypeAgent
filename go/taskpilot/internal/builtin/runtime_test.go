package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/retry"
)

func TestRuntimeRegistryHasNoDuplicates(t *testing.T) {
	// RuntimeRegistry wires every builtin; a panic here means two entries
	// claim the same name across the executor and task registries.
	_ = RuntimeRegistry()
}

func TestRegisterRejectsDuplicateExecutor(t *testing.T) {
	rt := &Runtime{executors: map[string]Executor{}, tasks: map[string]Task{}}
	noop := func(context.Context, map[string]any, Context) (any, error) { return nil, nil }
	rt.Register("dup", noop)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate executor registration")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "already registered as an executor") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	rt.Register("dup", noop)
}

func TestRegisterRejectsCrossMapName(t *testing.T) {
	rt := &Runtime{executors: map[string]Executor{}, tasks: map[string]Task{}}
	noop := func(context.Context, map[string]any, Context) (any, error) { return nil, nil }
	rt.RegisterTask(&stubTask{name: "clash"})
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when executor name collides with a task")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "already registered as a task") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	rt.Register("clash", noop)
}

func TestRegisterTaskRejectsCrossMapName(t *testing.T) {
	rt := &Runtime{executors: map[string]Executor{}, tasks: map[string]Task{}}
	noop := func(context.Context, map[string]any, Context) (any, error) { return nil, nil }
	rt.Register("clash", noop)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when task name collides with an executor")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "already registered as an executor") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	rt.RegisterTask(&stubTask{name: "clash"})
}

type stubTask struct {
	BaseTask
	name string
}

func (t *stubTask) Spec() model.TaskSpec { return model.TaskSpec{Name: t.name} }

func (t *stubTask) Execute(context.Context, map[string]any, Context) (any, error) {
	return nil, nil
}

func (t *stubTask) Retry(map[string]any) (retry.Options, error) { return retry.Options{}, nil }
