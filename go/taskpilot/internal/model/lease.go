package model

// Field names inside a lease envelope's inner object.
const (
	leaseKey        = "$lease"
	leaseKindField  = "kind"
	leaseIDField    = "id"
	leaseStateField = "state"
)

// Lease describes a claim on an external execution context. Kind selects the
// provider, ID names the backing instance, and State names its current state.
type Lease struct {
	Kind  string
	ID    string
	State string
}

// LeaseRef constructs a lease envelope.
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

// projectLeaseIdentity normalizes a value for lease identity by stripping
// runtime instance IDs while preserving provider kind and state.
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
