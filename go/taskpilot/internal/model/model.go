package model

import "github.com/microsoft/TypeAgent/go/taskpilot/internal/version"

// EngineVersion returns the engine compatibility version. It shares the
// internal/version source used by the CLI, so requiresEngine checks and version
// reporting cannot drift. It is a function rather than a package-global var so
// other packages cannot mutate it.
func EngineVersion() string { return version.Version() }

// DocumentKind and DocumentVersion are the canonical identity values every
// taskpilot document must declare. Verify and tests reference these rather than
// re-encoding the literals.
const (
	DocumentKind    = "taskpilot"
	DocumentVersion = 1
)

// Document is the top-level taskpilot definition: an identity header (Kind,
// Version), optional engine requirement and shared schemas/constants, an entry
// task, and the named tasks the engine can run.
type Document struct {
	Kind           string                 `json:"kind" yaml:"kind"`
	Version        int                    `json:"version" yaml:"version"`
	RequiresEngine string                 `json:"requiresEngine,omitempty" yaml:"requiresEngine,omitempty"`
	Entry          string                 `json:"entry" yaml:"entry"`
	Schemas        map[string]any         `json:"schemas,omitempty" yaml:"schemas,omitempty"`
	Constants      map[string]ConstantDef `json:"constants,omitempty" yaml:"constants,omitempty"`
	Tasks          map[string]TaskDef     `json:"tasks" yaml:"tasks"`
}

// ConstantDef is a named document constant: an optional Schema describing its
// shape and the literal Value made available to task templates.
type ConstantDef struct {
	Schema any `json:"schema,omitempty" yaml:"schema,omitempty"`
	Value  any `json:"value" yaml:"value"`
}

// TaskDef declares a task's input and output contracts and an optional Graph
// that implements it by orchestrating other tasks.
type TaskDef struct {
	Version      string `json:"version,omitempty" yaml:"version,omitempty"`
	InputSchema  any    `json:"inputSchema" yaml:"inputSchema"`
	OutputSchema any    `json:"outputSchema" yaml:"outputSchema"`
	Graph        *Graph `json:"graph,omitempty" yaml:"graph,omitempty"`
}

// Graph implements a task as a set of named Nodes wired by dependencies, with
// Output selecting the value the graph returns to its caller.
type Graph struct {
	Nodes  map[string]Node `json:"nodes" yaml:"nodes"`
	Output any             `json:"output" yaml:"output"`
}

// Node is one step in a Graph: it invokes a task (or, via ForEach/Loop, a
// repeated body) with templated Inputs, after the nodes in DependsOn complete.
type Node struct {
	Task           string         `json:"task" yaml:"task"`
	Version        string         `json:"version,omitempty" yaml:"version,omitempty"`
	Inputs         map[string]any `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	OutputSchema   any            `json:"outputSchema,omitempty" yaml:"outputSchema,omitempty"`
	DependsOn      []string       `json:"dependsOn,omitempty" yaml:"dependsOn,omitempty"`
	MaxConcurrency int            `json:"maxConcurrency,omitempty" yaml:"maxConcurrency,omitempty"`
	ForEach        *ForEachSpec   `json:"forEach,omitempty" yaml:"forEach,omitempty"`
	Loop           *LoopSpec      `json:"loop,omitempty" yaml:"loop,omitempty"`
}

// NodeMode names the single execution shape a node takes. A node is exactly one
// of these variants; mixing them (e.g. loop plus forEach) is not a valid shape.
type NodeMode string

const (
	NodeModeTask    NodeMode = "task"
	NodeModeForEach NodeMode = "forEach"
	NodeModeLoop    NodeMode = "loop"
)

// DeclaredModes lists the execution variants a node sets. A bare task counts
// as the task variant, so callers can detect invalid mixed modes.
func (n Node) DeclaredModes() []NodeMode {
	var modes []NodeMode
	if n.Loop != nil {
		modes = append(modes, NodeModeLoop)
	}
	if n.ForEach != nil {
		modes = append(modes, NodeModeForEach)
	}
	if n.ForEach == nil && n.Loop == nil {
		modes = append(modes, NodeModeTask)
	}
	return modes
}

// Mode returns the node's execution variant, resolving loop before forEach
// before a plain task. Callers should reject mixed loop/forEach shapes before
// relying on the result.
func (n Node) Mode() NodeMode {
	switch {
	case n.Loop != nil:
		return NodeModeLoop
	case n.ForEach != nil:
		return NodeModeForEach
	default:
		return NodeModeTask
	}
}

// ForEachSpec fans a node's task out over Items, with optional MaxConcurrency
// bounding how many invocations run at once.
type ForEachSpec struct {
	Items          any `json:"items" yaml:"items"`
	MaxConcurrency any `json:"maxConcurrency,omitempty" yaml:"maxConcurrency,omitempty"`
}

// RefTemplates returns the ForEachSpec fields whose templates may reference
// other graph nodes. Dependency and run-ID walkers share this field list.
func (f *ForEachSpec) RefTemplates() []TemplateField {
	return []TemplateField{
		{Name: "items", Value: f.Items},
		{Name: "maxConcurrency", Value: f.MaxConcurrency},
	}
}

// LoopSpec runs BodyTask repeatedly up to MaxIterations, threading State across
// iterations and stopping once ContinueWhen no longer holds.
type LoopSpec struct {
	MaxIterations any            `json:"maxIterations" yaml:"maxIterations"`
	State         map[string]any `json:"state,omitempty" yaml:"state,omitempty"`
	BodyTask      string         `json:"bodyTask" yaml:"bodyTask"`
	Inputs        map[string]any `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	ContinueWhen  any            `json:"continueWhen" yaml:"continueWhen"`
}

// TemplateField pairs a template-bearing field value with the name used to
// report it in diagnostics.
type TemplateField struct {
	Name  string
	Value any
}

// RefTemplates returns the LoopSpec fields walked for `$from: node` references.
// State and ContinueWhen are loop-local controls and are excluded from both
// dependency and run-ID analyses.
func (l *LoopSpec) RefTemplates() []TemplateField {
	return []TemplateField{
		{Name: "inputs", Value: l.Inputs},
		{Name: "maxIterations", Value: l.MaxIterations},
	}
}

// LeaseOutputKey is the output name under which a task emits a lease. It is the
// single spelling shared by the builtins that emit leases and by verification,
// which uses it to tell a reference that carries a lease from one that merely
// reads the same node's ordinary result.
const LeaseOutputKey = "lease"

// TaskSpec is a registry's view of a callable task: its Name, Version, and the
// InputSchema callers must satisfy, plus how it participates in a lease chain.
type TaskSpec struct {
	Name        string
	Version     string
	InputSchema any
	// LeaseInputs names the input properties that carry a lease. A node that
	// binds one of them to another node's output consumes that lease.
	LeaseInputs []string
	// EmitsLease reports whether the task produces a lease. A task with no
	// LeaseInputs is an acquisition root; a task with a bound LeaseInput threads
	// the consumed lease to its successor.
	EmitsLease bool
	// AlwaysRun disables engine memoization because serving the task from cache
	// would skip a required process-local side effect.
	AlwaysRun bool
}

// Registry resolves task names to their specs, exposing single lookups via Get
// and the full set via All.
type Registry interface {
	Get(name string) (TaskSpec, bool)
	All() []TaskSpec
}
