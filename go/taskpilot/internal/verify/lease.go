package verify

import (
	"fmt"
	"sort"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

// validateLeases enforces the linear discipline on lease values: a lease is
// consumed exactly once, and every chain ends in a terminal task.
func validateLeases(r *collector, reg model.Registry, _ *model.Document, taskName string, g *model.Graph) {
	emits := map[string]bool{}
	consumedBy := map[string][]string{}
	potentialEmitters := potentialLeaseEmitters(reg, g)

	for _, id := range sortedNodeIDs(g.Nodes) {
		node := g.Nodes[id]
		spec, known := reg.Get(node.Task)
		rejectWholeLeaseRefs(r, taskName, "inputs", id, node.Inputs, potentialEmitters)

		declared := map[string]bool{}
		if known {
			for _, name := range spec.LeaseInputs {
				for _, ref := range leaseRefsIn(node.Inputs[name]) {
					declared[ref] = true
				}
			}
		}
		accounted := map[string]bool{}
		for ref := range declared {
			accounted[ref] = true
		}
		for _, producer := range leaseRefsIn(node.Inputs) {
			if !declared[producer] {
				r.err("tasks.%s.nodes.%s: binds the lease from node %q to an input that is not a lease input of task %q; a lease may only be bound to a declared lease input", taskName, id, producer, node.Task)
			}
		}

		if node.ForEach != nil {
			for _, tf := range node.ForEach.RefTemplates() {
				rejectWholeLeaseRefs(r, taskName, "forEach."+tf.Name, id, tf.Value, potentialEmitters)
				for _, producer := range leaseRefsIn(tf.Value) {
					accounted[producer] = true
					r.err("tasks.%s.nodes.%s.forEach.%s: a forEach node may not acquire or consume a lease; its body runs repeatedly and concurrently against a single context", taskName, id, tf.Name)
				}
			}
		}
		if node.Loop != nil {
			for _, tf := range loopLeaseTemplates(node.Loop) {
				rejectWholeLeaseRefs(r, taskName, "loop."+tf.Name, id, tf.Value, potentialEmitters)
				for _, producer := range leaseRefsIn(tf.Value) {
					accounted[producer] = true
					r.err("tasks.%s.nodes.%s.loop.%s: a loop node may not consume a lease from node %q; loop body tasks cannot declare lease inputs, and the body runs repeatedly against a single context", taskName, id, tf.Name, producer)
				}
			}
		}

		consumed := sortedKeys(declared)
		for _, producer := range sortedKeys(accounted) {
			consumedBy[producer] = append(consumedBy[producer], id)
		}

		if len(consumed) > 1 && spec.EmitsLease {
			r.err("tasks.%s.nodes.%s: consumes %d leases but can emit only one successor; bind at most one lease per node", taskName, id, len(consumed))
		}
		if len(consumed) > 0 || (known && spec.EmitsLease && len(spec.LeaseInputs) == 0) {
			if node.ForEach != nil || node.Loop != nil {
				r.err("tasks.%s.nodes.%s: a %s node may not acquire or consume a lease; its body runs repeatedly and concurrently against a single context", taskName, id, node.Mode())
			}
		}

		switch {
		case known && spec.EmitsLease && len(spec.LeaseInputs) == 0:
			emits[id] = true
		case known && !spec.EmitsLease && len(spec.LeaseInputs) > 0:
			if len(consumed) == 0 {
				r.err("tasks.%s.nodes.%s: %s ends a lease chain but no lease is bound to it", taskName, id, node.Task)
			}
		case known && spec.EmitsLease && len(consumed) > 0:
			emits[id] = true
		}
	}

	rejectWholeLeaseRefs(r, taskName, "output", "<output>", g.Output, potentialEmitters)

	for _, producer := range sortedKeys(emits) {
		consumers := consumedBy[producer]
		switch {
		case len(consumers) == 0:
			r.err("tasks.%s.nodes.%s: emits a lease that no node consumes; every lease must be consumed exactly once and its chain ended by a terminal task", taskName, producer)
		case len(consumers) > 1:
			sort.Strings(consumers)
			r.err("tasks.%s.nodes.%s: emits a lease consumed by %d nodes (%s); a lease must be consumed exactly once so access stays exclusive", taskName, producer, len(consumers), joinQuoted(consumers))
		}
	}
}

func potentialLeaseEmitters(reg model.Registry, g *model.Graph) map[string]bool {
	out := map[string]bool{}
	for id, node := range g.Nodes {
		spec, known := reg.Get(node.Task)
		if known && spec.EmitsLease && len(spec.LeaseInputs) == 0 {
			out[id] = true
		}
	}
	changed := true
	for changed {
		changed = false
		for id, node := range g.Nodes {
			if out[id] {
				continue
			}
			spec, known := reg.Get(node.Task)
			if !known || !spec.EmitsLease {
				continue
			}
			for _, input := range spec.LeaseInputs {
				for _, producer := range leaseRefsIn(node.Inputs[input]) {
					if out[producer] {
						out[id] = true
						changed = true
					}
				}
			}
		}
	}
	return out
}

func rejectWholeLeaseRefs(r *collector, taskName, field, nodeID string, value any, emitters map[string]bool) {
	for _, producer := range wholeNodeRefsIn(value) {
		if emitters[producer] {
			r.err("tasks.%s.nodes.%s.%s: references lease-emitting node %q without selecting a path; select path: [lease] to consume the lease or an ordinary result path to avoid copying it", taskName, nodeID, field, producer)
		}
	}
}

func loopLeaseTemplates(loop *model.LoopSpec) []model.TemplateField {
	fields := loop.RefTemplates()
	for _, tf := range fields {
		if tf.Name == "state" {
			return fields
		}
	}
	return append(fields, model.TemplateField{Name: "state", Value: loop.State})
}

// leaseRefsIn returns the nodes whose lease output is explicitly selected.
func leaseRefsIn(v any) []string {
	var refs []string
	seen := map[string]bool{}
	var walk func(any)
	walk = func(x any) {
		switch value := x.(type) {
		case map[string]any:
			if from, _ := value["$from"].(string); from == "node" {
				if node, _ := value["node"].(string); node != "" && selectsLease(value["path"]) && !seen[node] {
					seen[node] = true
					refs = append(refs, node)
				}
			}
			for _, item := range value {
				walk(item)
			}
		case []any:
			for _, item := range value {
				walk(item)
			}
		}
	}
	walk(v)
	sort.Strings(refs)
	return refs
}

func wholeNodeRefsIn(v any) []string {
	var refs []string
	seen := map[string]bool{}
	var walk func(any)
	walk = func(x any) {
		switch value := x.(type) {
		case map[string]any:
			if from, _ := value["$from"].(string); from == "node" {
				node, _ := value["node"].(string)
				path, hasPath := value["path"]
				segments, _ := path.([]any)
				if node != "" && (!hasPath || len(segments) == 0) && !seen[node] {
					seen[node] = true
					refs = append(refs, node)
				}
			}
			for _, item := range value {
				walk(item)
			}
		case []any:
			for _, item := range value {
				walk(item)
			}
		}
	}
	walk(v)
	sort.Strings(refs)
	return refs
}

func selectsLease(path any) bool {
	segs, ok := path.([]any)
	if !ok || len(segs) == 0 {
		return false
	}
	first, _ := segs[0].(string)
	return first == model.LeaseOutputKey
}

func sortedNodeIDs(nodes map[string]model.Node) []string {
	out := make([]string, 0, len(nodes))
	for id := range nodes {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func joinQuoted(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%q", item)
	}
	return out
}
