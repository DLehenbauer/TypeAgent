package verify

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/schema"
)

// VerifiedDocument is a document that has passed validation. Because Document
// only ever returns one alongside a nil error, holding a *VerifiedDocument is
// itself proof of validity: callers need not inspect an error list. Graphs
// reports the resolved task graph structure keyed by task name.
type VerifiedDocument struct {
	Doc    *model.Document
	Graphs map[string]GraphInfo
}

// ValidationError reports one or more reasons a document failed validation.
// Document returns it (and a nil *VerifiedDocument) whenever validation finds at
// least one problem.
type ValidationError struct {
	Errors []string
}

// Error formats the validation failures as a multi-line message.
func (e *ValidationError) Error() string {
	return "verification failed:\n" + strings.Join(e.Errors, "\n")
}

// collector accumulates validation errors and the per-task graph reports while
// Document walks a document. It is an internal scratch value: success and
// failure are surfaced to callers as *VerifiedDocument or *ValidationError.
type collector struct {
	graphs map[string]GraphInfo
	errors []string
}

// GraphInfo reports a task graph's resolved structure: Order is a topological
// ordering of node IDs and Edges lists the dependency edges in sorted order.
// When a cycle is detected, Order holds a partial ordering of only the acyclic
// nodes reachable before the cycle blocks progress; the nodes participating in
// (or downstream of) the cycle are omitted, so Order is empty only when every
// node is caught in a cycle.
type GraphInfo struct {
	Order []string `json:"order"`
	Edges []Edge   `json:"edges"`
}

// Edge is a directed dependency from one graph node to another, meaning To runs
// after From completes.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Document validates doc against the registry. On success it returns a
// *VerifiedDocument and a nil error; when validation finds any problem it
// returns a nil *VerifiedDocument and a *ValidationError listing every reason.
func Document(doc *model.Document, reg model.Registry) (*VerifiedDocument, error) {
	r := &collector{graphs: map[string]GraphInfo{}}
	if doc.Kind != model.DocumentKind {
		r.err("kind: expected %s", model.DocumentKind)
	}
	if doc.Version != model.DocumentVersion {
		r.err("version: expected %d", model.DocumentVersion)
	}
	if doc.RequiresEngine != "" {
		if err := validateEngineConstraint(doc.RequiresEngine); err != nil {
			r.err("requiresEngine: %v", err)
		}
	}
	if doc.Entry == "" {
		r.err("entry: required")
	} else if _, ok := doc.Tasks[doc.Entry]; !ok {
		r.err("entry: task %q not found", doc.Entry)
	}
	if len(doc.Tasks) == 0 {
		r.err("tasks: at least one task is required")
	}
	// Validate constants before task graphs so schema errors are reported early.
	for name, c := range doc.Constants {
		if err := schema.Compile(c.Schema); err != nil {
			r.err("constants.%s.schema: %v", name, err)
			continue
		}
		if err := schema.Validate(c.Schema, c.Value); err != nil {
			r.err("constants.%s: %v", name, err)
		}
	}
	for name, task := range doc.Tasks {
		if task.InputSchema == nil {
			r.err("tasks.%s.inputSchema: required", name)
		} else if err := schema.Compile(task.InputSchema); err != nil {
			r.err("tasks.%s.inputSchema: %v", name, err)
		}
		if task.OutputSchema == nil {
			r.err("tasks.%s.outputSchema: required", name)
		} else if err := schema.Compile(task.OutputSchema); err != nil {
			r.err("tasks.%s.outputSchema: %v", name, err)
		}
		if task.Graph == nil {
			r.err("tasks.%s.graph: graph tasks require a graph", name)
			continue
		}
		info := validateGraph(r, doc, reg, name, task.Graph)
		r.graphs[name] = info
	}
	validateTaskCallCycles(r, doc)
	if len(r.errors) > 0 {
		return nil, &ValidationError{Errors: r.errors}
	}
	return &VerifiedDocument{Doc: doc, Graphs: r.graphs}, nil
}

// validateGraph validates a task graph and returns its dependency order and edges.
func validateGraph(r *collector, doc *model.Document, reg model.Registry, taskName string, g *model.Graph) GraphInfo {
	info := GraphInfo{}
	if g == nil {
		return info
	}
	if len(g.Nodes) == 0 {
		if g.Output == nil {
			r.err("tasks.%s.graph.output: required when graph has no nodes", taskName)
		}
		return info
	}
	deps := map[string]map[string]bool{}
	for id, node := range g.Nodes {
		if modes := node.DeclaredModes(); len(modes) > 1 {
			names := make([]string, len(modes))
			for i, m := range modes {
				names[i] = string(m)
			}
			r.err("tasks.%s.nodes.%s: exactly one of task, forEach, or loop allowed; found %s", taskName, id, strings.Join(names, ", "))
		}
		if node.Loop != nil {
			if node.Loop.BodyTask == "" {
				r.err("tasks.%s.nodes.%s.loop.bodyTask: required", taskName, id)
			} else if _, ok := doc.Tasks[node.Loop.BodyTask]; !ok {
				r.err("tasks.%s.nodes.%s.loop.bodyTask: task %q not found", taskName, id, node.Loop.BodyTask)
			}
		} else if node.Task == "" {
			r.err("tasks.%s.nodes.%s.task: required", taskName, id)
		} else if _, ok := reg.Get(node.Task); !ok {
			if _, ok := doc.Tasks[node.Task]; !ok {
				r.err("tasks.%s.nodes.%s.task: task %q not found", taskName, id, node.Task)
			}
		}
		deps[id] = map[string]bool{}
		if node.OutputSchema != nil {
			if err := schema.Compile(node.OutputSchema); err != nil {
				r.err("tasks.%s.nodes.%s.outputSchema: %v", taskName, id, err)
			}
		}
		checkRefs := func(field, kind string, refs []string) {
			for _, ref := range refs {
				if _, ok := g.Nodes[ref]; !ok {
					r.err("tasks.%s.nodes.%s.%s: %s %q not found", taskName, id, field, kind, ref)
				}
				deps[id][ref] = true
			}
		}
		checkRefs("dependsOn", "node", node.DependsOn)
		checkRefs("inputs", "node ref", collectNodeRefs(node.Inputs))
		if node.ForEach != nil {
			for _, tf := range node.ForEach.RefTemplates() {
				checkRefs("forEach."+tf.Name, "node ref", collectNodeRefs(tf.Value))
			}
		}
		if node.Loop != nil {
			for _, tf := range node.Loop.RefTemplates() {
				checkRefs("loop."+tf.Name, "node ref", collectNodeRefs(tf.Value))
			}
		}
	}
	order, cycle := topo(deps)
	if len(cycle) > 0 {
		r.err("tasks.%s.graph: cycle detected: %s", taskName, strings.Join(cycle, " -> "))
	}
	validateLeases(r, reg, doc, taskName, g)
	info.Order = order
	for to, froms := range deps {
		for from := range froms {
			info.Edges = append(info.Edges, Edge{From: from, To: to})
		}
	}
	sort.Slice(info.Edges, func(i, j int) bool {
		if info.Edges[i].From == info.Edges[j].From {
			return info.Edges[i].To < info.Edges[j].To
		}
		return info.Edges[i].From < info.Edges[j].From
	})
	return info
}

// collectNodeRefs collects node references embedded in nested input values.
func collectNodeRefs(v any) []string {
	var refs []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if from, _ := t["$from"].(string); from == "node" {
				if n, _ := t["node"].(string); n != "" {
					refs = append(refs, n)
				}
			}
			for _, v := range t {
				walk(v)
			}
		case []any:
			for _, v := range t {
				walk(v)
			}
		}
	}
	walk(v)
	return refs
}

// topo builds a topological ordering for the dependency graph and reports nodes
// left after acyclic progress stops.
func topo(deps map[string]map[string]bool) ([]string, []string) {
	in := map[string]int{}
	next := map[string][]string{}
	for n := range deps {
		in[n] = 0
	}
	for n, ds := range deps {
		for d := range ds {
			in[n]++
			next[d] = append(next[d], n)
		}
	}
	var ready []string
	for n, c := range in {
		if c == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)
		for _, m := range next[n] {
			in[m]--
			if in[m] == 0 {
				ready = append(ready, m)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(deps) {
		var cycle []string
		for n, c := range in {
			if c > 0 {
				cycle = append(cycle, n)
			}
		}
		sort.Strings(cycle)
		return order, cycle
	}
	return order, nil
}

// validateTaskCallCycles detects recursive graph task calls after per-task validation.
func validateTaskCallCycles(r *collector, doc *model.Document) {
	graph := map[string]map[string]bool{}
	for name, task := range doc.Tasks {
		graph[name] = map[string]bool{}
		if task.Graph == nil {
			continue
		}
		for _, node := range task.Graph.Nodes {
			if node.Loop != nil && node.Loop.BodyTask != "" {
				if _, ok := doc.Tasks[node.Loop.BodyTask]; ok {
					graph[name][node.Loop.BodyTask] = true
				}
			}
			if _, ok := doc.Tasks[node.Task]; ok {
				graph[name][node.Task] = true
			}
		}
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var stack []string
	var dfs func(string)
	dfs = func(n string) {
		if visiting[n] {
			r.err("tasks: recursive graph task call detected: %s -> %s", strings.Join(stack, " -> "), n)
			return
		}
		if visited[n] {
			return
		}
		visiting[n] = true
		stack = append(stack, n)
		for child := range graph[n] {
			dfs(child)
		}
		stack = stack[:len(stack)-1]
		visiting[n] = false
		visited[n] = true
	}
	for n := range graph {
		dfs(n)
	}
}

// validateEngineConstraint ensures the running engine satisfies a documented
// version constraint.
func validateEngineConstraint(constraint string) error {
	parts := strings.Fields(constraint)
	if len(parts) == 0 {
		return fmt.Errorf("constraint is empty")
	}
	engineVersion := model.EngineVersion()
	engine, err := parseSemver(engineVersion)
	if err != nil {
		return fmt.Errorf("engine version %q is invalid: %w", engineVersion, err)
	}
	for _, part := range parts {
		op, versionText := splitComparator(part)
		if versionText == "" {
			return fmt.Errorf("invalid comparator %q", part)
		}
		target, err := parseSemver(versionText)
		if err != nil {
			return fmt.Errorf("invalid semver in comparator %q: %w", part, err)
		}
		cmp := compareSemver(engine, target)
		for _, c := range comparators {
			if c.op != op {
				continue
			}
			if !c.matches(cmp) {
				return fmt.Errorf("engine %s does not satisfy %s", engineVersion, constraint)
			}
			break
		}
	}
	return nil
}

// comparators is the single source of truth for supported constraint
// operators. Order matters: longer operators must precede their prefixes so
// splitComparator matches ">=" before ">".
var comparators = []struct {
	op      string
	matches func(cmp int) bool
}{
	{">=", func(cmp int) bool { return cmp >= 0 }},
	{"<=", func(cmp int) bool { return cmp <= 0 }},
	{">", func(cmp int) bool { return cmp > 0 }},
	{"<", func(cmp int) bool { return cmp < 0 }},
	{"=", func(cmp int) bool { return cmp == 0 }},
}

// splitComparator splits a version comparator into its operator and version.
func splitComparator(part string) (string, string) {
	for _, c := range comparators {
		if strings.HasPrefix(part, c.op) {
			return c.op, strings.TrimSpace(part[len(c.op):])
		}
	}
	return "=", strings.TrimSpace(part)
}

// semver represents a semantic version with an optional prerelease suffix.
type semver struct {
	major int
	minor int
	patch int
	pre   []string
}

// parseSemver parses a semantic version string into the internal form.
func parseSemver(raw string) (semver, error) {
	v := strings.TrimSpace(raw)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return semver{}, fmt.Errorf("empty version")
	}
	buildSplit := strings.SplitN(v, "+", 2)
	main := buildSplit[0]
	preSplit := strings.SplitN(main, "-", 2)
	core := preSplit[0]
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("expected major.minor.patch")
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return semver{}, fmt.Errorf("invalid major version")
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor < 0 {
		return semver{}, fmt.Errorf("invalid minor version")
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil || patch < 0 {
		return semver{}, fmt.Errorf("invalid patch version")
	}
	parsed := semver{major: major, minor: minor, patch: patch}
	if len(preSplit) == 2 {
		if preSplit[1] == "" {
			return semver{}, fmt.Errorf("invalid pre-release")
		}
		parsed.pre = strings.Split(preSplit[1], ".")
	}
	return parsed, nil
}

// compareSemver compares two semantic versions using the taskpilot engine ordering.
func compareSemver(a, b semver) int {
	if a.major != b.major {
		if a.major < b.major {
			return -1
		}
		return 1
	}
	if a.minor != b.minor {
		if a.minor < b.minor {
			return -1
		}
		return 1
	}
	if a.patch != b.patch {
		if a.patch < b.patch {
			return -1
		}
		return 1
	}
	if len(a.pre) == 0 && len(b.pre) == 0 {
		return 0
	}
	if len(a.pre) == 0 {
		return 1
	}
	if len(b.pre) == 0 {
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x := a.pre[i]
		y := b.pre[i]
		xNum, xErr := strconv.Atoi(x)
		yNum, yErr := strconv.Atoi(y)
		switch {
		case xErr == nil && yErr == nil:
			if xNum < yNum {
				return -1
			}
			if xNum > yNum {
				return 1
			}
		case xErr == nil && yErr != nil:
			return -1
		case xErr != nil && yErr == nil:
			return 1
		default:
			if x < y {
				return -1
			}
			if x > y {
				return 1
			}
		}
	}
	if len(a.pre) < len(b.pre) {
		return -1
	}
	if len(a.pre) > len(b.pre) {
		return 1
	}
	return 0
}

// err records a validation error that will be returned to the caller.
func (r *collector) err(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
