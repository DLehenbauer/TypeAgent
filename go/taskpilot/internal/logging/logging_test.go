package logging_test

import (
	"context"
	"errors"
	"os"
	"testing"

	tflog "github.com/microsoft/TypeAgent/go/taskpilot/internal/logging"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func TestProcessorWritesStartAndEndRecords(t *testing.T) {
	dir := t.TempDir()
	proc, err := tflog.Open(dir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	tp, shutdown := telemetry.Setup(proc)
	tr := tp.Tracer("test")

	_, span := tr.Start(context.Background(), telemetry.SpanNode, trace.WithAttributes(
		attribute.String(telemetry.AttrNodeName, "render"),
	))
	span.SetStatus(codes.Ok, "")
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(tflog.LogPath(dir, "run-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records, err := tflog.ReadSpanRecords(f)
	if err != nil {
		t.Fatal(err)
	}
	var phases []string
	var endRec telemetry.SpanRecord
	for _, rec := range records {
		phases = append(phases, rec.Phase)
		if rec.Phase == telemetry.PhaseEnd {
			endRec = rec
		}
	}

	if len(phases) != 2 || phases[0] != telemetry.PhaseStart || phases[1] != telemetry.PhaseEnd {
		t.Fatalf("phases = %v, want [start end]", phases)
	}
	if endRec.Attributes[telemetry.AttrNodeName].String() != "render" {
		t.Fatalf("end record node name = %v", endRec.Attributes[telemetry.AttrNodeName])
	}
	if endRec.Status != telemetry.StatusOK {
		t.Fatalf("end record status = %q, want ok", endRec.Status)
	}
	if endRec.EndTime == nil {
		t.Fatal("end record missing EndTime")
	}
}

func TestProcessorLifecycleInvariants(t *testing.T) {
	// A nil (never-opened) processor is a lifecycle violation: every method must
	// panic rather than silently accept the illegal state.
	var p *tflog.Processor
	assertPanics(t, "ForceFlush on nil", func() { _ = p.ForceFlush(context.Background()) })
	assertPanics(t, "Shutdown on nil", func() { _ = p.Shutdown(context.Background()) })

	// A zero-value processor was never constructed via Open and is equally illegal.
	assertPanics(t, "ForceFlush on zero value", func() { _ = new(tflog.Processor).ForceFlush(context.Background()) })

	// After Shutdown the processor is closed; ForceFlush and a second Shutdown
	// must report the closed state explicitly instead of pretending success.
	proc, err := tflog.Open(t.TempDir(), "run-closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := proc.ForceFlush(context.Background()); !errors.Is(err, tflog.ErrClosed) {
		t.Fatalf("ForceFlush after close = %v, want ErrClosed", err)
	}
	if err := proc.Shutdown(context.Background()); !errors.Is(err, tflog.ErrClosed) {
		t.Fatalf("second Shutdown = %v, want ErrClosed", err)
	}
}

func assertPanics(t *testing.T, desc string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: expected panic, got none", desc)
		}
	}()
	fn()
}
