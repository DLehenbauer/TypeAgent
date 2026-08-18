package builtin

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

// structToSchema builds a draft-2020-12 JSON Schema for a builtin input type.
// It panics on failure because builtin schemas are compile-time-known.
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

// withMinimum sets minimum on a property in a struct-derived schema.
// It panics if the generated schema does not contain the named property.
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
