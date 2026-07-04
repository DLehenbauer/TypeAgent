package telemetry_test

import (
	"context"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestRecordFromSpanMapsAttributesStatusAndEvents(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	tr := tp.Tracer("test")

	_, span := tr.Start(context.Background(), telemetry.SpanNode, trace.WithAttributes(
		attribute.String(telemetry.AttrNodeName, "n1"),
		attribute.String(telemetry.AttrTask, "template.expand"),
	))
	span.AddEvent(telemetry.EventClaimWait)
	span.SetStatus(codes.Error, "boom")
	span.End()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	rec := telemetry.EndRecord(ended[0])

	if rec.Phase != telemetry.PhaseEnd {
		t.Fatalf("phase = %q, want end", rec.Phase)
	}
	if rec.Name != telemetry.SpanNode {
		t.Fatalf("name = %q, want %q", rec.Name, telemetry.SpanNode)
	}
	if rec.Attributes[telemetry.AttrNodeName].String() != "n1" {
		t.Fatalf("node name attr = %v, want n1", rec.Attributes[telemetry.AttrNodeName])
	}
	if rec.Attributes[telemetry.AttrTask].String() != "template.expand" {
		t.Fatalf("task attr = %v", rec.Attributes[telemetry.AttrTask])
	}
	if rec.Status != telemetry.StatusError || rec.StatusMsg != "boom" {
		t.Fatalf("status = %q msg = %q, want error/boom", rec.Status, rec.StatusMsg)
	}
	if rec.EndTime == nil {
		t.Fatal("end record missing EndTime")
	}
	if len(rec.Events) != 1 || rec.Events[0].Name != telemetry.EventClaimWait {
		t.Fatalf("events = %#v, want one %q", rec.Events, telemetry.EventClaimWait)
	}
	if rec.TraceID == "" || rec.SpanID == "" {
		t.Fatal("record missing trace/span id")
	}
}

func TestSetupReturnsProviderAndShutdown(t *testing.T) {
	prevPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(prevPropagator) })

	tp, shutdown := telemetry.Setup()
	if tp == nil {
		t.Fatal("Setup returned nil provider")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
