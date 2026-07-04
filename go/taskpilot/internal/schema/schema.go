// Package schema validates JSON values against JSON Schema documents. It is a
// thin adapter over github.com/google/jsonschema-go/jsonschema: schemas flow
// through the system as decoded JSON (map[string]any, []any, ...), so this
// package round-trips them into the library's typed *jsonschema.Schema at
// validation time. Draft 2020-12 and draft-07 are supported.
package schema

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// Validate checks value against schema, returning a descriptive error for the
// first violation found. A nil schema accepts any value and returns nil. The
// schema must be a JSON object (map[string]any); any other shape, including a
// JSON boolean schema, is rejected as an error rather than silently accepted.
// An otherwise invalid schema is reported as an error.
//
// This compiles the schema on every call. Callers that repeatedly validate the
// same schemas (such as the engine across parallel node execution) should hold
// a *Validator, which memoizes compiled schemas.
func Validate(schema any, value any) error {
	rs, err := compile(schema)
	if err != nil {
		return err
	}
	return validateResolved(rs, value)
}

// Compile reports whether schema is a well-formed JSON Schema, returning a
// descriptive error otherwise. A nil schema is well-formed. The schema must be
// a JSON object; a non-object schema (such as a JSON boolean) is rejected.
// Callers such as the verifier use this to reject malformed task/constant
// schemas up front.
func Compile(schema any) error {
	_, err := compile(schema)
	return err
}

// Validator validates values against schemas, memoizing compiled schemas keyed
// by their canonical JSON. Go's json.Marshal emits object keys in sorted order,
// so structurally identical schemas share a key. *jsonschema.Resolved is safe
// for concurrent Validate, so a single Validator can be shared across the
// engine's parallel node execution. The zero Validator is ready to use, but
// prefer NewValidator for clarity.
type Validator struct {
	cache sync.Map // string -> compiled
}

type compiled struct {
	rs  *jsonschema.Resolved
	err error
}

// NewValidator returns a Validator with an empty compilation cache.
func NewValidator() *Validator {
	return &Validator{}
}

// Validate behaves like the package-level Validate but reuses previously
// compiled schemas from this Validator's cache.
func (v *Validator) Validate(schema any, value any) error {
	rs, err := v.compile(schema)
	if err != nil {
		return err
	}
	return validateResolved(rs, value)
}

// compile converts schema into a resolved, validation-ready schema, memoizing
// the result in this Validator. A nil schema yields a nil *Resolved.
func (v *Validator) compile(schema any) (*jsonschema.Resolved, error) {
	if schema == nil {
		return nil, nil
	}
	obj, ok := schema.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema must be a JSON object, got %T", schema)
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	key := string(raw)
	if cv, ok := v.cache.Load(key); ok {
		c := cv.(compiled)
		return c.rs, c.err
	}
	rs, err := resolve(raw)
	v.cache.Store(key, compiled{rs: rs, err: err})
	return rs, err
}

// validateResolved runs a resolved schema against value. A nil resolved schema
// (from a nil source schema) accepts any value.
func validateResolved(rs *jsonschema.Resolved, value any) error {
	if rs == nil {
		return nil
	}
	norm, err := normalize(value)
	if err != nil {
		return err
	}
	return rs.Validate(norm)
}

// compile converts schema into a resolved, validation-ready schema without
// caching. A nil schema yields a nil *Resolved, which callers treat as
// accept-any. The schema must be a JSON object (map[string]any); any other
// shape is rejected with an error, so a bare JSON boolean cannot silently stand
// in as an accept-all/reject-all schema.
func compile(schema any) (*jsonschema.Resolved, error) {
	if schema == nil {
		return nil, nil
	}
	obj, ok := schema.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema must be a JSON object, got %T", schema)
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	return resolve(raw)
}

// resolve parses and resolves canonical schema JSON into a *jsonschema.Resolved.
func resolve(raw []byte) (*jsonschema.Resolved, error) {
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	return s.Resolve(nil)
}

// normalize converts an arbitrary Go value into its JSON-native form (float64,
// string, bool, nil, map[string]any, []any) as expected by jsonschema-go's
// validator. This folds Go-typed inputs (int, structs, named slices) into the
// shapes the validator understands.
func normalize(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal value: %w", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse value: %w", err)
	}
	return out, nil
}

// ApplyDefaults fills missing top-level object properties of value with the
// `default` declared on their schema. It returns the same map (allocating one
// only when value is nil and a default is applied), and never overwrites a key
// that is already present. Schemas without object properties, and properties
// without a default, are left untouched. Callers run this before Validate so a
// defaulted property still satisfies a `required` constraint.
//
// This intentionally does not delegate to jsonschema-go's ApplyDefaults, which
// by design skips required properties; taskpilot applies defaults precisely so
// that a required-but-defaulted property passes validation.
func ApplyDefaults(schema any, value map[string]any) map[string]any {
	s, ok := schema.(map[string]any)
	if !ok {
		return value
	}
	props, ok := s["properties"].(map[string]any)
	if !ok {
		return value
	}
	for name, propSchema := range props {
		if _, exists := value[name]; exists {
			continue
		}
		p, ok := propSchema.(map[string]any)
		if !ok {
			continue
		}
		def, ok := p["default"]
		if !ok {
			continue
		}
		if value == nil {
			value = map[string]any{}
		}
		value[name] = def
	}
	return value
}
