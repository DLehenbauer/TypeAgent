package builtin

import (
	"context"
	"fmt"
	"sort"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/retry"
)

type schemaRegistry struct {
	tasks map[string]model.TaskSpec
}

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

func (r *schemaRegistry) Get(name string) (model.TaskSpec, bool) {
	spec, ok := r.tasks[name]
	return spec, ok
}

func (r *schemaRegistry) All() []model.TaskSpec {
	out := make([]model.TaskSpec, 0, len(r.tasks))
	for _, spec := range r.tasks {
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

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

type Runtime struct {
	executors map[string]Executor
	tasks     map[string]Task
	providers *provider.Set
}

type Context struct {
	RunID     string
	DryRun    bool
	Providers *provider.Set
}

type Executor func(context.Context, map[string]any, Context) (any, error)

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

// reserve panics when name is already registered in either registry, keeping
// executor and task names globally unique and unambiguous at Execute time.
func (r *Runtime) reserve(name string) {
	if _, ok := r.executors[name]; ok {
		panic(fmt.Sprintf("builtin %q already registered as an executor", name))
	}
	if _, ok := r.tasks[name]; ok {
		panic(fmt.Sprintf("builtin %q already registered as a task", name))
	}
}

// SetProviders injects the provider set made available to tasks during execution.
func (r *Runtime) SetProviders(p *provider.Set) { r.providers = p }

// externalDigesters maps each content-addressed builtin to the function that
// digests the external filesystem state its output depends on. Tasks absent from
// this map have no external state and are cached on their inputs alone.
var externalDigesters = map[string]func(map[string]any) (string, error){
	fileReadJSONSpec.Name:      fileContentDigest,
	fileExistsSpec.Name:        fileExistsDigest,
	fileGlobSpec().Name:        globDigest,
	fileRefSpec.Name:           fileContentDigest,
	fileRefGlobSpec.Name:       globContentDigest,
	sourceFingerprintSpec.Name: func(input map[string]any) (string, error) { return fingerprint(asString(input["path"])) },
	templateExpandSpec.Name:    templatePathDigest,
}

// ExternalDigest returns a digest of the external state task depends on and
// ok=true when task is content-addressed; otherwise it returns ok=false. The
// digest folds into the node identity so the node re-runs when that state
// changes and cache-hits when it does not.
func (r *Runtime) ExternalDigest(task string, input map[string]any) (string, bool, error) {
	digester, ok := externalDigesters[task]
	if !ok {
		return "", false, nil
	}
	digest, err := digester(input)
	if err != nil {
		return "", false, err
	}
	return digest, true, nil
}

func (r *Runtime) Execute(ctx context.Context, name string, input map[string]any, taskCtx Context) (any, error) {
	taskCtx.Providers = r.providers
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
