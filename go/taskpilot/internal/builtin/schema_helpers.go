package builtin

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

// structToSchema builds a JSON Schema (draft 2020-12) for t via jsonschema-go's
// reflection-based inference. Exported fields become properties keyed by their
// JSON name; fields not marked "omitempty"/"omitzero" become required, and
// additional properties are disallowed, so the generated schema mirrors the Go
// struct exactly. The result is returned as a map[string]any so builtin input
// schemas flow through the model uniformly with document-authored schemas.
//
// It panics on failure: the argument is a compile-time-known builtin input
// type, so a failure indicates a programming error, not runtime input.
func structToSchema(t reflect.Type) map[string]any {
	s, err := jsonschema.ForType(t, nil)
	if err != nil {
		panic(fmt.Sprintf("schema for %s: %v", t, err))
	}
	raw, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("marshal schema for %s: %v", t, err))
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		panic(fmt.Sprintf("decode schema for %s: %v", t, err))
	}
	return m
}

// withMinimum sets `minimum` on the named property of a struct-derived schema,
// pushing a lower-bound invariant to the schema boundary so validation rejects
// out-of-range values before the executor runs. It panics if the property is
// absent: the schema and property name are compile-time-known, so a mismatch is
// a programming error rather than runtime input.
func withMinimum(schema map[string]any, property string, min float64) map[string]any {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		panic(fmt.Sprintf("schema has no properties, cannot constrain %q", property))
	}
	p, ok := props[property].(map[string]any)
	if !ok {
		panic(fmt.Sprintf("schema has no property %q to constrain", property))
	}
	p["minimum"] = min
	return schema
}
