package builtin

import (
	"context"
	"fmt"
	"sort"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/retry"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
)

// schemaRegistry stores builtin task specs by name.
type schemaRegistry struct {
	tasks map[string]model.TaskSpec
}

// SchemaRegistry returns the builtin registry used by the engine and CLI when
// validating tasks.
func SchemaRegistry() model.Registry {
	reg := &schemaRegistry{tasks: map[string]model.TaskSpec{}}
	for _, spec := range specs() {
		reg.tasks[spec.Name] = spec
	}
	for _, t := range taskObjects() {
		spec := t.Spec()
		reg.tasks[spec.Name] = spec
	}
	return reg
}

// Get returns the registered task spec for name when it exists.
func (r *schemaRegistry) Get(name string) (model.TaskSpec, bool) {
	spec, ok := r.tasks[name]
	return spec, ok
}

// All returns every registered spec in name order.
func (r *schemaRegistry) All() []model.TaskSpec {
	out := make([]model.TaskSpec, 0, len(r.tasks))
	for _, spec := range r.tasks {
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// specs builds the builtin task spec list used by both registry constructors.
func specs() []model.TaskSpec {
	return []model.TaskSpec{
		templateExpandSpec,
		jsonProjectSpec(),
		jsonParseSpec,
		jsonValidateSpec,
		jsonStringifySpec,
		stringJoinSpec,
		stringSplitSpec(),
		pathJoinSpec(),
		fileReadJSONSpec,
		fileWriteSpec,
		fileExistsSpec,
		fileGlobSpec(),
		fileRefSpec,
		fileRefGlobSpec,
		sourceFingerprintSpec,
		runIDSpec,
		listMapSpec,
		listReduceSpec,
		jsonlParseSpec,
		jsonlStringifySpec,
		listFlattenSpec,
		listChunkSpec,
	}
}

// Runtime stores builtin executors and task implementations for a single process.
type Runtime struct {
	executors map[string]Executor
	tasks     map[string]Task
	services  *Services
}

// Context carries execution metadata and runtime services for a task.
type Context struct {
	RunID string
	// NodeID is the content-addressed identity of the executing node. A
	// checkpointing target run uses it as the durable state name.
	NodeID   string
	DryRun   bool
	Services *Services
}

// Executor runs a builtin task with the current context and input.
type Executor func(context.Context, map[string]any, Context) (any, error)

// RuntimeRegistry builds the default runtime used by the engine and tests.
func RuntimeRegistry() *Runtime {
	rt := &Runtime{executors: map[string]Executor{}, tasks: map[string]Task{}}
	rt.Register(templateExpandSpec.Name, expandTemplate)
	rt.Register(jsonProjectSpec().Name, projectJSON)
	rt.Register(jsonParseSpec.Name, parseJSON)
	rt.Register(jsonValidateSpec.Name, validateJSON)
	rt.Register(jsonStringifySpec.Name, stringifyJSON)
	rt.Register(stringJoinSpec.Name, joinString)
	rt.Register(stringSplitSpec().Name, splitString)
	rt.Register(pathJoinSpec().Name, joinPath)
	rt.Register(fileReadJSONSpec.Name, readJSONFile)
	rt.Register(fileWriteSpec.Name, writeFile)
	rt.Register(fileExistsSpec.Name, fileExists)
	rt.Register(fileGlobSpec().Name, globFiles)
	rt.Register(fileRefSpec.Name, makeFileRef)
	rt.Register(fileRefGlobSpec.Name, makeFileRefGlob)
	rt.Register(sourceFingerprintSpec.Name, fingerprintSource)
	rt.Register(runIDSpec.Name, runID)
	rt.Register(listMapSpec.Name, mapList)
	rt.Register(listReduceSpec.Name, reduceList)
	rt.Register(jsonlParseSpec.Name, parseJSONL)
	rt.Register(jsonlStringifySpec.Name, stringifyJSONL)
	rt.Register(listFlattenSpec.Name, flattenList)
	rt.Register(listChunkSpec.Name, chunkList)
	for _, t := range taskObjects() {
		rt.RegisterTask(t)
	}
	return rt
}

// Register binds an executor to name. It panics if name is already claimed by
// another executor or a task, since ambiguous builtin registrations are a
// programming error in the static registry.
func (r *Runtime) Register(name string, exec Executor) {
	r.reserve(name)
	r.executors[name] = exec
}

// RegisterTask binds a Task under its spec name. It panics if the name is
// already claimed by an executor or another task.
func (r *Runtime) RegisterTask(t Task) {
	name := t.Spec().Name
	r.reserve(name)
	r.tasks[name] = t
}

// reserve guards against duplicate builtin names so execution remains
// unambiguous.
func (r *Runtime) reserve(name string) {
	if _, ok := r.executors[name]; ok {
		panic(fmt.Sprintf("builtin %q already registered as an executor", name))
	}
	if _, ok := r.tasks[name]; ok {
		panic(fmt.Sprintf("builtin %q already registered as a task", name))
	}
}

// SetServices injects the runtime integrations made available to tasks.
func (r *Runtime) SetServices(s *Services) { r.services = s }

// SetProviders installs providers, lazily creating Services when needed.
func (r *Runtime) SetProviders(p *provider.Set) {
	if r.services == nil {
		r.services = &Services{}
	}
	r.services.Providers = p
}

// SetTargets installs a target registry, lazily creating Services when needed.
func (r *Runtime) SetTargets(targets *target.Registry) {
	if r.services == nil {
		r.services = &Services{}
	}
	r.services.Targets = targets
}

// externalDigesters maps each content-addressed builtin to the function that
// digests the external filesystem state its output depends on.
var externalDigesters = map[string]func(map[string]any) (string, error){
	fileReadJSONSpec.Name:      fileContentDigest,
	fileExistsSpec.Name:        fileExistsDigest,
	fileGlobSpec().Name:        globDigest,
	fileRefSpec.Name:           fileContentDigest,
	fileRefGlobSpec.Name:       globContentDigest,
	sourceFingerprintSpec.Name: func(input map[string]any) (string, error) { return fingerprint(asString(input["path"])) },
	templateExpandSpec.Name:    templatePathDigest,
}

// CacheBehavior reports whether a node may be memoized and the external-state
// digest that participates in node identity.
func (r *Runtime) CacheBehavior(task string, input map[string]any) (memoize bool, digest string, err error) {
	if spec, ok := r.taskSpec(task); ok && spec.AlwaysRun {
		return false, "", nil
	}
	if task == pwshRunTaskName {
		// Lease-bound runs operate on live target state and are always executed.
		if _, bound := input[PwshRunsOnInput]; bound {
			return false, "", nil
		}
		if raw, present := input["cache"]; present {
			cache, ok := raw.(bool)
			if !ok {
				return false, "", fmt.Errorf("%s: cache must be a boolean", task)
			}
			if !cache {
				return false, "", nil
			}
		}
	}
	digester, ok := externalDigesters[task]
	if !ok {
		return true, "", nil
	}
	digest, err = digester(input)
	if err != nil {
		return false, "", err
	}
	return true, digest, nil
}

// workspaceIdentityFields are the workspace options that change what a guest
// script can see. They are an allowlist rather than a credential denylist so a
// new secret field cannot silently become part of a cache key.
var workspaceIdentityFields = []string{"uncPath", "drive"}

// IdentityInputs strips operational task inputs that must not invalidate
// content-addressed target state.
func (r *Runtime) IdentityInputs(task string, input map[string]any) map[string]any {
	projected := model.ProjectLeaseIdentityMap(input)
	if task == LeaseAcquireTaskName {
		// The failure policy affects cleanup, not the durable target state.
		delete(projected, LeaseKeepOnFailureInput)
		if options, ok := projected[LeaseOptionsInput].(map[string]any); ok {
			// Guest settings affect only how the target is reached. Workspace
			// location affects what scripts and checkpoints observe, so its
			// location is kept while its credentials are not.
			delete(options, "guest")
			if workspace, ok := options["workspace"].(map[string]any); ok {
				options["workspace"] = retainFields(workspace, workspaceIdentityFields)
			}
		}
	}
	return projected
}

// retainFields returns the subset of m named by fields, omitting absent keys.
func retainFields(m map[string]any, fields []string) map[string]any {
	out := make(map[string]any, len(fields))
	for _, field := range fields {
		if value, ok := m[field]; ok {
			out[field] = value
		}
	}
	return out
}

// taskSpec looks up a spec, including builtins registered as bare executors.
func (r *Runtime) taskSpec(name string) (model.TaskSpec, bool) {
	if task, ok := r.tasks[name]; ok {
		return task.Spec(), true
	}
	for _, spec := range specs() {
		if spec.Name == name {
			return spec, true
		}
	}
	return model.TaskSpec{}, false
}

// Execute runs a named task or executor; Task implementations use managed
// retries.
func (r *Runtime) Execute(ctx context.Context, name string, input map[string]any, taskCtx Context) (any, error) {
	taskCtx.Services = r.services
	if t, ok := r.tasks[name]; ok {
		opts, err := t.Retry(input)
		if err != nil {
			return nil, err
		}
		return retry.Run(ctx, opts, func(ctx context.Context) (any, error) {
			return t.Execute(ctx, input, taskCtx)
		})
	}
	exec, ok := r.executors[name]
	if !ok {
		return nil, fmt.Errorf("unknown builtin %q", name)
	}
	return exec(ctx, input, taskCtx)
}
