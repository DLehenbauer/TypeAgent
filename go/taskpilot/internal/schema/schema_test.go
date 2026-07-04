package schema

import "testing"

func TestValidateObjectRequired(t *testing.T) {
	s := map[string]any{
		"type":     "object",
		"required": []any{"name"},
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
	}
	if err := Validate(s, map[string]any{"name": "world"}); err != nil {
		t.Fatal(err)
	}
	if err := Validate(s, map[string]any{}); err == nil {
		t.Fatal("expected required property error")
	}
}

func TestApplyDefaultsFillsMissing(t *testing.T) {
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "string", "default": "x"},
			"b": map[string]any{"type": "integer", "default": 10},
			"c": map[string]any{"type": "string"},
		},
	}
	out := ApplyDefaults(s, map[string]any{"a": "given"})
	if out["a"] != "given" {
		t.Fatalf("existing value overwritten: %v", out["a"])
	}
	if out["b"] != 10 {
		t.Fatalf("default not applied for b: %v", out["b"])
	}
	if _, exists := out["c"]; exists {
		t.Fatalf("c has no default and should stay absent: %v", out["c"])
	}
}

func TestApplyDefaultsNilValue(t *testing.T) {
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "string", "default": "x"},
		},
	}
	out := ApplyDefaults(s, nil)
	if out["a"] != "x" {
		t.Fatalf("default not applied to nil map: %v", out["a"])
	}
}

func TestApplyDefaultsNonObjectSchema(t *testing.T) {
	if out := ApplyDefaults(nil, map[string]any{"a": 1}); out["a"] != 1 {
		t.Fatalf("nil schema should pass value through: %v", out)
	}
	if out := ApplyDefaults(map[string]any{"type": "string"}, nil); out != nil {
		t.Fatalf("schema without properties should return value unchanged: %v", out)
	}
}

func TestApplyDefaultsThenValidateSatisfiesRequired(t *testing.T) {
	s := map[string]any{
		"type":     "object",
		"required": []any{"name"},
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "default": "world"},
		},
	}
	if err := Validate(s, ApplyDefaults(s, map[string]any{})); err != nil {
		t.Fatalf("defaulted value should satisfy required: %v", err)
	}
}

func TestValidateNilSchemaAcceptsAnything(t *testing.T) {
	if err := Validate(nil, map[string]any{"anything": 1}); err != nil {
		t.Fatalf("nil schema should accept any value: %v", err)
	}
}

func TestValidateNormalizesGoNativeValues(t *testing.T) {
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count": map[string]any{"type": "integer"},
		},
		"required": []any{"count"},
	}
	// A Go int must normalize to a JSON number that satisfies "integer".
	if err := Validate(s, map[string]any{"count": 3}); err != nil {
		t.Fatalf("Go int should satisfy integer schema: %v", err)
	}
	if err := Validate(s, map[string]any{"count": "nope"}); err == nil {
		t.Fatal("string value should violate integer schema")
	}
}

func TestValidateRejectsMalformedSchema(t *testing.T) {
	// "type" must be a string or array of strings, not a number.
	bad := map[string]any{"type": 5}
	if err := Validate(bad, map[string]any{}); err == nil {
		t.Fatal("expected error validating against a malformed schema")
	}
}

func TestValidateRejectsNonObjectSchema(t *testing.T) {
	// JSON boolean schemas are valid per the spec (true accepts anything,
	// false rejects everything) but taskpilot requires object schemas, so both
	// must fail explicitly rather than silently passing or emitting a cryptic
	// parse error.
	for _, bad := range []any{true, false, "string schema", 42, []any{"array"}} {
		if err := Validate(bad, map[string]any{"any": 1}); err == nil {
			t.Fatalf("expected non-object schema %#v to be rejected", bad)
		}
		if err := Compile(bad); err == nil {
			t.Fatalf("expected Compile to reject non-object schema %#v", bad)
		}
	}
}

func TestCompile(t *testing.T) {
	if err := Compile(nil); err != nil {
		t.Fatalf("nil schema should be well-formed: %v", err)
	}
	valid := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
		"additionalProperties": false,
	}
	if err := Compile(valid); err != nil {
		t.Fatalf("valid schema should compile: %v", err)
	}
	if err := Compile(map[string]any{"type": 5}); err == nil {
		t.Fatal("expected error compiling a malformed schema")
	}
}

func TestValidatorMemoizesAndValidates(t *testing.T) {
	v := NewValidator()
	s := map[string]any{
		"type":     "object",
		"required": []any{"name"},
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
	}
	// First call compiles and caches; a structurally identical schema on the
	// second call must reuse the cached compilation and behave identically.
	if err := v.Validate(s, map[string]any{"name": "world"}); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	same := map[string]any{
		"type":     "object",
		"required": []any{"name"},
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
	}
	if err := v.Validate(same, map[string]any{}); err == nil {
		t.Fatal("cached schema should still enforce required property")
	}
	// A nil schema accepts anything, and errors surface for bad schemas.
	if err := v.Validate(nil, map[string]any{"any": 1}); err != nil {
		t.Fatalf("nil schema should accept any value: %v", err)
	}
	if err := v.Validate(true, map[string]any{}); err == nil {
		t.Fatal("non-object schema should be rejected")
	}
	if err := v.Validate(map[string]any{"type": 5}, map[string]any{}); err == nil {
		t.Fatal("malformed schema should be rejected")
	}
}
