package logging

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/telemetry"
)

type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }

// newFailingProcessor returns a Processor whose encoder always fails, backed by
// a real (working) file so the lifecycle sync/close paths still run.
func newFailingProcessor(t *testing.T, err error) *Processor {
	t.Helper()
	f, ferr := os.CreateTemp(t.TempDir(), "run-*.jsonl")
	if ferr != nil {
		t.Fatal(ferr)
	}
	t.Cleanup(func() { _ = f.Close() })
	return &Processor{f: f, enc: json.NewEncoder(errWriter{err: err})}
}

func TestWriteLatchesEncodeErrorSurfacedByForceFlush(t *testing.T) {
	boom := errors.New("boom")
	p := newFailingProcessor(t, boom)

	p.write(telemetry.SpanRecord{Phase: telemetry.PhaseStart})

	if err := p.ForceFlush(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("ForceFlush err = %v, want to wrap %v", err, boom)
	}
	// The latch clears once surfaced.
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("second ForceFlush err = %v, want nil", err)
	}
}

func TestWriteLatchesEncodeErrorSurfacedByShutdown(t *testing.T) {
	boom := errors.New("boom")
	p := newFailingProcessor(t, boom)

	p.write(telemetry.SpanRecord{Phase: telemetry.PhaseEnd})

	if err := p.Shutdown(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Shutdown err = %v, want to wrap %v", err, boom)
	}
}

func TestWriteLatchesFirstEncodeError(t *testing.T) {
	first := errors.New("first")
	p := newFailingProcessor(t, first)

	p.write(telemetry.SpanRecord{Phase: telemetry.PhaseStart})
	// Swap the underlying error; the first latched error must win.
	p.enc = json.NewEncoder(errWriter{err: errors.New("second")})
	p.write(telemetry.SpanRecord{Phase: telemetry.PhaseEnd})

	if err := p.Shutdown(context.Background()); !errors.Is(err, first) {
		t.Fatalf("Shutdown err = %v, want to wrap %v", err, first)
	}
}

func TestForceFlushNoErrorWhenWritesSucceed(t *testing.T) {
	dir := t.TempDir()
	p, err := Open(dir, "run-ok")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	p.write(telemetry.SpanRecord{Phase: telemetry.PhaseStart})

	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush err = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "run-ok.jsonl")); err != nil {
		t.Fatal(err)
	}
}
