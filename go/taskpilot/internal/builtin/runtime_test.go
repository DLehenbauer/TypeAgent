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

// Lease identity keeps a workspace allowlist rather than a credential
// denylist, so a workspace field added later cannot silently enter the cache
// key or leak a credential into it.
func TestIdentityInputsKeepsOnlyWorkspaceLocation(t *testing.T) {
	rt := RuntimeRegistry()
	projected := rt.IdentityInputs(LeaseAcquireTaskName, map[string]any{
		LeaseKeepOnFailureInput: true,
		LeaseOptionsInput: map[string]any{
			"baseImage": "base.vhdx",
			"guest":     map[string]any{"username": "u", "passwordEnv": "P"},
			"workspace": map[string]any{
				"uncPath":     `\\host\share`,
				"drive":       "Z:",
				"username":    "worker",
				"passwordEnv": "WORKSPACE_PASSWORD",
				"apiToken":    "a-field-added-later",
			},
		},
	})

	if _, ok := projected[LeaseKeepOnFailureInput]; ok {
		t.Fatal("keepOnFailure leaked into target identity")
	}
	options, ok := projected[LeaseOptionsInput].(map[string]any)
	if !ok {
		t.Fatalf("options = %#v, want an object", projected[LeaseOptionsInput])
	}
	if _, ok := options["guest"]; ok {
		t.Fatal("guest connection settings leaked into target identity")
	}
	if options["baseImage"] != "base.vhdx" {
		t.Fatalf("baseImage = %#v, want it preserved", options["baseImage"])
	}
	workspace, ok := options["workspace"].(map[string]any)
	if !ok {
		t.Fatalf("workspace = %#v, want an object", options["workspace"])
	}
	want := map[string]any{"uncPath": `\\host\share`, "drive": "Z:"}
	if len(workspace) != len(want) {
		t.Fatalf("workspace identity = %#v, want %#v", workspace, want)
	}
	for key, value := range want {
		if workspace[key] != value {
			t.Fatalf("workspace[%q] = %#v, want %#v", key, workspace[key], value)
		}
	}
}
