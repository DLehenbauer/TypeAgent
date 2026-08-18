// Package telemetry holds taskpilot's OpenTelemetry conventions: the attribute
// and span names the engine emits, a TracerProvider constructor, and a
// serializable SpanRecord that span processors (e.g. the JSONL log) and the CLI
// renderer share as their on-the-wire schema.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TracerName is the instrumentation scope used by the engine.
const TracerName = "github.com/microsoft/TypeAgent/go/taskpilot/internal/engine"

// Span names.
const (
	SpanRun  = "taskpilot.run"
	SpanNode = "taskpilot.node"
)

// Attribute keys. Kept under the taskpilot.* namespace so they don't collide
// with standard semantic conventions when exported to a collector.
const (
	AttrRunID       = "taskpilot.run.id"
	AttrEntry       = "taskpilot.entry"
	AttrDryRun      = "taskpilot.dry_run"
	AttrNodeName    = "taskpilot.node.name"
	AttrNodeID      = "taskpilot.node.id"
	AttrTask        = "taskpilot.task"
	AttrVersion     = "taskpilot.version"
	AttrCacheStatus = "taskpilot.cache.status"
	AttrCachePath   = "taskpilot.cache.path"
	AttrStage       = "taskpilot.stage"
	// AttrSpanKind is the stable discriminator the log renderer switches on. The
	// span Name carries a human-readable, higher-cardinality label for viewers,
	// so it cannot be relied on for routing.
	AttrSpanKind = "taskpilot.span.kind"
	// AttrForEachIndex is the 0-based position of a forEach item span among its
	// siblings; AttrForEachCount is the total number of items fanned out.
	AttrForEachIndex = "taskpilot.foreach.index"
	AttrForEachCount = "taskpilot.foreach.count"
)

// Span kind attribute values.
const (
	SpanKindRun     = "run"
	SpanKindNode    = "node"
	SpanKindForEach = "forEach"
	SpanKindLoop    = "loop"
)

// Span event names.
const (
	EventClaimWait     = "taskpilot.cache.claim_wait"
	EventClaimAcquired = "taskpilot.cache.claim_acquired"
)

// SpanRecord.Phase values marking a span's lifecycle position. These are the
// canonical phase tokens shared by the producer (StartRecord/EndRecord), the JSONL
// serializer, and the CLI renderer.
const (
	PhaseStart = "start"
	PhaseEnd   = "end"
)

// SpanRecord.Status values. An empty Status means unset. These are the
// canonical status tokens shared by the producer, serializer, and renderer.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// Cache status attribute values.
const (
	CacheStatusHit          = "hit"
	CacheStatusMiss         = "miss"
	CacheStatusDryRun       = "dry_run"
	CacheStatusNonCacheable = "noncacheable"
)

// Setup builds a TracerProvider wired to the given span processors. It returns
// the provider and its shutdown function; callers must invoke shutdown to flush
// processors before reading persisted spans. Registering a process-global
// propagator is left to the CLI boundary so this helper is safe to reuse.
func Setup(processors ...sdktrace.SpanProcessor) (*sdktrace.TracerProvider, func(context.Context) error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", "taskpilot")),
	)
	if err != nil {
		// Falls back to the SDK default resource when the merge fails.
		res = resource.Default()
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	for _, p := range processors {
		if p != nil {
			opts = append(opts, sdktrace.WithSpanProcessor(p))
		}
	}
	tp := sdktrace.NewTracerProvider(opts...)
	return tp, tp.Shutdown
}

// SpanRecord is the serializable form of a span at a lifecycle phase. The JSONL
// span processor writes one record per line; the CLI renderer decodes them.
type SpanRecord struct {
	Phase      string               `json:"phase"` // "start" or "end"
	Name       string               `json:"name"`
	TraceID    string               `json:"traceId"`
	SpanID     string               `json:"spanId"`
	ParentID   string               `json:"parentSpanId,omitempty"`
	StartTime  time.Time            `json:"startTime"`
	EndTime    *time.Time           `json:"endTime,omitempty"`
	DurationMs int64                `json:"durationMs,omitempty"`
	Status     string               `json:"status,omitempty"` // "ok", "error", or "" (unset)
	StatusMsg  string               `json:"statusMessage,omitempty"`
	Attributes map[string]AttrValue `json:"attributes,omitempty"`
	Events     []SpanEvent          `json:"events,omitempty"`
}

// SpanEvent is the serializable form of a span event (e.g. a cache claim wait).
type SpanEvent struct {
	Name       string               `json:"name"`
	Time       time.Time            `json:"time"`
	Attributes map[string]AttrValue `json:"attributes,omitempty"`
}

// AttrKind names which variant of an AttrValue is populated. It mirrors the
// closed set of OpenTelemetry attribute value types taskpilot can emit, so an
// attribute is never carried as an untyped any.
type AttrKind string

const (
	AttrKindString      AttrKind = "string"
	AttrKindBool        AttrKind = "bool"
	AttrKindInt         AttrKind = "int"
	AttrKindFloat       AttrKind = "float"
	AttrKindStringSlice AttrKind = "stringSlice"
	AttrKindBoolSlice   AttrKind = "boolSlice"
	AttrKindIntSlice    AttrKind = "intSlice"
	AttrKindFloatSlice  AttrKind = "floatSlice"
)

// AttrValue is a telemetry attribute value drawn from the closed OpenTelemetry
// primitive domain. Kind selects which field is meaningful. It marshals to the
// bare JSON scalar (or array) so the on-disk JSONL stays compact and readable.
type AttrValue struct {
	Kind       AttrKind
	Str        string
	Int        int64
	Float      float64
	Bool       bool
	StrSlice   []string
	IntSlice   []int64
	FloatSlice []float64
	BoolSlice  []bool
}

// StringValue builds a string-kinded AttrValue.
func StringValue(s string) AttrValue { return AttrValue{Kind: AttrKindString, Str: s} }

// BoolValue builds a bool-kinded AttrValue.
func BoolValue(b bool) AttrValue { return AttrValue{Kind: AttrKindBool, Bool: b} }

// IntValue builds an int-kinded AttrValue.
func IntValue(i int64) AttrValue { return AttrValue{Kind: AttrKindInt, Int: i} }

// FloatValue builds a float-kinded AttrValue.
func FloatValue(f float64) AttrValue { return AttrValue{Kind: AttrKindFloat, Float: f} }

// StringSliceValue builds a string-slice-kinded AttrValue.
func StringSliceValue(s []string) AttrValue {
	return AttrValue{Kind: AttrKindStringSlice, StrSlice: s}
}

// BoolSliceValue builds a bool-slice-kinded AttrValue.
func BoolSliceValue(b []bool) AttrValue { return AttrValue{Kind: AttrKindBoolSlice, BoolSlice: b} }

// IntSliceValue builds an int-slice-kinded AttrValue.
func IntSliceValue(i []int64) AttrValue { return AttrValue{Kind: AttrKindIntSlice, IntSlice: i} }

// FloatSliceValue builds a float-slice-kinded AttrValue.
func FloatSliceValue(f []float64) AttrValue {
	return AttrValue{Kind: AttrKindFloatSlice, FloatSlice: f}
}

// String renders the value for human-facing log output.
func (v AttrValue) String() string {
	switch v.Kind {
	case AttrKindString:
		return v.Str
	case AttrKindBool:
		return strconv.FormatBool(v.Bool)
	case AttrKindInt:
		return strconv.FormatInt(v.Int, 10)
	case AttrKindFloat:
		return strconv.FormatFloat(v.Float, 'g', -1, 64)
	case AttrKindStringSlice:
		return fmt.Sprint(v.StrSlice)
	case AttrKindBoolSlice:
		return fmt.Sprint(v.BoolSlice)
	case AttrKindIntSlice:
		return fmt.Sprint(v.IntSlice)
	case AttrKindFloatSlice:
		return fmt.Sprint(v.FloatSlice)
	default:
		return ""
	}
}

// MarshalJSON writes the bare scalar (or array) for the populated variant.
func (v AttrValue) MarshalJSON() ([]byte, error) {
	switch v.Kind {
	case AttrKindString:
		return json.Marshal(v.Str)
	case AttrKindBool:
		return json.Marshal(v.Bool)
	case AttrKindInt:
		return json.Marshal(v.Int)
	case AttrKindFloat:
		return json.Marshal(v.Float)
	case AttrKindStringSlice:
		return json.Marshal(v.StrSlice)
	case AttrKindBoolSlice:
		return json.Marshal(v.BoolSlice)
	case AttrKindIntSlice:
		return json.Marshal(v.IntSlice)
	case AttrKindFloatSlice:
		return json.Marshal(v.FloatSlice)
	default:
		return []byte("null"), nil
	}
}

// UnmarshalJSON classifies the bare JSON scalar (or array) into a variant
// without ever decoding through an any, keeping the value domain closed.
func (v *AttrValue) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		*v = AttrValue{}
		return nil
	}
	switch data[0] {
	case '"':
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*v = StringValue(s)
	case 't', 'f':
		var b bool
		if err := json.Unmarshal(data, &b); err != nil {
			return err
		}
		*v = BoolValue(b)
	case '[':
		return v.unmarshalSlice(data)
	default:
		if bytes.ContainsAny(data, ".eE") {
			var f float64
			if err := json.Unmarshal(data, &f); err != nil {
				return err
			}
			*v = FloatValue(f)
			return nil
		}
		var i int64
		if err := json.Unmarshal(data, &i); err != nil {
			return err
		}
		*v = IntValue(i)
	}
	return nil
}

// unmarshalSlice decodes the concrete slice variant from a JSON array while
// keeping the value domain closed.
func (v *AttrValue) unmarshalSlice(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		// Empty arrays have no element from which to infer a slice type.
		*v = StringSliceValue(nil)
		return nil
	}
	first := bytes.TrimSpace(raw[0])
	// Infers the slice element type from the first element so the decode stays closed.
	switch first[0] {
	case '"':
		var s []string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*v = StringSliceValue(s)
	case 't', 'f':
		var b []bool
		if err := json.Unmarshal(data, &b); err != nil {
			return err
		}
		*v = BoolSliceValue(b)
	default:
		if bytes.ContainsAny(data, ".eE") {
			var f []float64
			if err := json.Unmarshal(data, &f); err != nil {
				return err
			}
			*v = FloatSliceValue(f)
			return nil
		}
		var i []int64
		if err := json.Unmarshal(data, &i); err != nil {
			return err
		}
		*v = IntSliceValue(i)
	}
	return nil
}

// StartRecord builds the lifecycle "start" SpanRecord for s.
func StartRecord(s sdktrace.ReadOnlySpan) SpanRecord {
	return recordFromSpan(PhaseStart, s)
}

// EndRecord builds the lifecycle "end" SpanRecord for s.
func EndRecord(s sdktrace.ReadOnlySpan) SpanRecord {
	return recordFromSpan(PhaseEnd, s)
}

// recordFromSpan converts a span at the given lifecycle phase into a
// serializable SpanRecord. It is reached only through StartRecord/EndRecord, so
// phase is always a valid token rather than a caller-supplied string.
func recordFromSpan(phase string, s sdktrace.ReadOnlySpan) SpanRecord {
	sc := s.SpanContext()
	rec := SpanRecord{
		Phase:      phase,
		Name:       s.Name(),
		TraceID:    sc.TraceID().String(),
		SpanID:     sc.SpanID().String(),
		StartTime:  s.StartTime(),
		Attributes: attrsToMap(s.Attributes()),
	}
	if parent := s.Parent(); parent.SpanID().IsValid() {
		rec.ParentID = parent.SpanID().String()
	}
	if phase == PhaseEnd {
		end := s.EndTime()
		rec.EndTime = &end
		rec.DurationMs = end.Sub(s.StartTime()).Milliseconds()
	}
	switch s.Status().Code {
	case codes.Ok:
		rec.Status = StatusOK
	case codes.Error:
		rec.Status = StatusError
		rec.StatusMsg = s.Status().Description
	}
	for _, ev := range s.Events() {
		rec.Events = append(rec.Events, SpanEvent{
			Name:       ev.Name,
			Time:       ev.Time,
			Attributes: attrsToMap(ev.Attributes),
		})
	}
	return rec
}

// attrsToMap maps OpenTelemetry attributes to the compact JSON-safe telemetry
// map used by the span serializer.
func attrsToMap(kvs []attribute.KeyValue) map[string]AttrValue {
	if len(kvs) == 0 {
		return nil
	}
	m := make(map[string]AttrValue, len(kvs))
	for _, kv := range kvs {
		m[string(kv.Key)] = attrValue(kv.Value)
	}
	return m
}

// attrValue converts an OpenTelemetry attribute value into the closed AttrValue
// domain. It is total over the primitive types; an invalid value degrades to
// its string emission rather than escaping the closed set.
func attrValue(v attribute.Value) AttrValue {
	switch v.Type() {
	case attribute.BOOL:
		return BoolValue(v.AsBool())
	case attribute.INT64:
		return IntValue(v.AsInt64())
	case attribute.FLOAT64:
		return FloatValue(v.AsFloat64())
	case attribute.STRING:
		return StringValue(v.AsString())
	case attribute.BOOLSLICE:
		return BoolSliceValue(v.AsBoolSlice())
	case attribute.INT64SLICE:
		return IntSliceValue(v.AsInt64Slice())
	case attribute.FLOAT64SLICE:
		return FloatSliceValue(v.AsFloat64Slice())
	case attribute.STRINGSLICE:
		return StringSliceValue(v.AsStringSlice())
	default:
		return StringValue(v.Emit())
	}
}
