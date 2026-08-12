package model

// Field names inside a lease envelope's inner object.
const (
	leaseKey        = "$lease"
	leaseKindField  = "kind"
	leaseIDField    = "id"
	leaseStateField = "state"
)

// A lease envelope has the shape
// {"$lease": {"kind": ..., "id": ..., "state": ...}}.
//
// A lease is a linear claim on an external execution context. It names which
// kind of context (hyperv, and later ssh, container, ...), which physical
// instance currently backs it, and the state that instance is at.
//
// Identity folds in {kind, state} only. The physical id is excluded
// so a graph is stable across different backing instances -- the same workflow
// cache-hits whether it ran on VM A or VM B -- and any credential material is
// excluded by never being carried here at all, so rotating a service password
// cannot invalidate cached node IDs.
//
// Lease describes a claim on an external execution context. Kind selects the
// provider that backs it, ID names the instance currently materializing it,
// and State names the state that instance is at.
type Lease struct {
	Kind  string
	ID    string
	State string
}

// LeaseRef builds a lease envelope.
func LeaseRef(l Lease) map[string]any {
	return map[string]any{leaseKey: map[string]any{
		leaseKindField:  l.Kind,
		leaseIDField:    l.ID,
		leaseStateField: l.State,
	}}
}

// AsLease reports whether v is a lease envelope and, if so, returns it.
func AsLease(v any) (Lease, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return Lease{}, false
	}
	inner, ok := m[leaseKey].(map[string]any)
	if !ok {
		return Lease{}, false
	}
	for _, field := range []string{leaseKindField, leaseIDField, leaseStateField} {
		if _, ok := inner[field].(string); !ok {
			return Lease{}, false
		}
	}
	if len(inner) != 3 {
		return Lease{}, false
	}
	str := func(f string) string { s, _ := inner[f].(string); return s }
	return Lease{
		Kind:  str(leaseKindField),
		ID:    str(leaseIDField),
		State: str(leaseStateField),
	}, true
}

// ProjectLeaseIdentityMap returns the task input shape used for node identity.
// Lease instance IDs are runtime allocation details and are removed; kind and
// state remain so a different target state invalidates downstream nodes.
func ProjectLeaseIdentityMap(inputs map[string]any) map[string]any {
	out := make(map[string]any, len(inputs))
	for key, value := range inputs {
		out[key] = projectLeaseIdentity(value)
	}
	return out
}

func projectLeaseIdentity(v any) any {
	if lease, ok := AsLease(v); ok {
		return map[string]any{leaseKey: map[string]any{
			leaseKindField:  lease.Kind,
			leaseStateField: lease.State,
		}}
	}
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = projectLeaseIdentity(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = projectLeaseIdentity(item)
		}
		return out
	default:
		return v
	}
}

// WithState returns a copy of l advanced to a state the backend confirmed it
// committed.
func (l Lease) WithState(state string) Lease {
	l.State = state
	return l
}
