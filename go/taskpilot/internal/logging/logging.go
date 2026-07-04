// Package logging persists a run's spans as a JSONL file. It implements the
// OpenTelemetry SpanProcessor interface so it can be registered on the
// TracerProvider alongside any other in-process consumers (e.g. a live TUI).
//
// One JSON line is written per span lifecycle phase: an OnStart record (so live
// progress can show a node starting) and an OnEnd record (carrying duration and
// status). Records use telemetry.SpanRecord as their schema.
package logging

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/telemetry"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Processor is a SpanProcessor that appends telemetry.SpanRecord lines to a
// per-run JSONL file. It is safe for concurrent use.
//
// A Processor is only valid once obtained from Open, and only until Shutdown
// closes its file. Every method enforces that lifecycle: a nil receiver or a
// zero-value Processor (never opened) is a programmer error and panics, while
// operating on an already-closed Processor is reported explicitly (ErrClosed
// from ForceFlush and Shutdown, a panic from a stray write). Illegal states are
// never silently accepted.
//
// OnStart and OnEnd cannot return errors (the SpanProcessor interface is
// fire-and-forget), so an encode failure is latched in writeErr and surfaced
// the next time ForceFlush or Shutdown is called.
type Processor struct {
	mu       sync.Mutex
	f        *os.File
	enc      *json.Encoder
	closed   bool
	writeErr error
}

var _ sdktrace.SpanProcessor = (*Processor)(nil)

// ErrClosed is returned by ForceFlush and Shutdown when they are called after a
// prior Shutdown has already closed the processor's file.
var ErrClosed = errors.New("logging: processor already closed")

// LogPath returns the JSONL file path for runID under dir. It is the single
// source of truth for the run log filename convention.
func LogPath(dir, runID string) string {
	return filepath.Join(dir, runID+".jsonl")
}

// Scan buffer limits for run-log JSONL. Records carry span attributes and event
// payloads, so a single line can exceed bufio's default token limit.
const (
	scanInitialBuffer = 64 * 1024
	scanMaxBuffer     = 10 * 1024 * 1024
)

// NewScanner returns a bufio.Scanner sized for run-log JSONL records. Every
// reader of the log shares these buffer limits so they tokenize identically.
func NewScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, scanInitialBuffer), scanMaxBuffer)
	return scanner
}

// DecodeSpanRecord decodes a single JSONL log line into a telemetry.SpanRecord.
// It is the one place that defines how a persisted line maps back to a record,
// so the CLI renderer, root-cause analysis, and tests all share identical
// decode semantics.
func DecodeSpanRecord(line []byte) (telemetry.SpanRecord, error) {
	var rec telemetry.SpanRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return telemetry.SpanRecord{}, err
	}
	return rec, nil
}

// ReadSpanRecords reads every span record from r, decoding one record per line
// via DecodeSpanRecord and skipping blank lines. It stops at the first
// malformed line.
func ReadSpanRecords(r io.Reader) ([]telemetry.SpanRecord, error) {
	scanner := NewScanner(r)
	var records []telemetry.SpanRecord
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		rec, err := DecodeSpanRecord(line)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// Open creates (or appends to) the JSONL file for runID under dir and returns a
// SpanProcessor that writes span records to it.
func Open(dir, runID string) (*Processor, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(LogPath(dir, runID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o666)
	if err != nil {
		return nil, err
	}
	return &Processor{f: f, enc: json.NewEncoder(f)}, nil
}

// OnStart writes a "start" record for the span.
func (p *Processor) OnStart(_ context.Context, s sdktrace.ReadWriteSpan) {
	p.write(telemetry.StartRecord(s))
}

// OnEnd writes an "end" record for the span.
func (p *Processor) OnEnd(s sdktrace.ReadOnlySpan) {
	p.write(telemetry.EndRecord(s))
}

func (p *Processor) write(rec telemetry.SpanRecord) {
	if p == nil {
		panic("logging: write on nil Processor")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requireOpen("write")
	if p.closed {
		panic("logging: write on closed Processor")
	}
	if err := p.enc.Encode(rec); err != nil && p.writeErr == nil {
		p.writeErr = fmt.Errorf("encode %s record: %w", rec.Phase, err)
	}
}

// requireOpen panics if the Processor was never constructed via Open. Callers
// must hold p.mu and must have already ruled out a nil receiver.
func (p *Processor) requireOpen(op string) {
	if p.enc == nil {
		panic("logging: " + op + " on Processor not created by Open")
	}
}

// ForceFlush syncs buffered file data to disk. It also surfaces any span encode
// failure latched since the last flush or shutdown. It returns ErrClosed if the
// processor has already been shut down.
func (p *Processor) ForceFlush(context.Context) error {
	if p == nil {
		panic("logging: ForceFlush on nil Processor")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requireOpen("ForceFlush")
	if p.closed {
		return ErrClosed
	}
	return errors.Join(p.takeWriteErr(), p.f.Sync())
}

// Shutdown flushes and closes the underlying file. It also surfaces any span
// encode failure latched during the run. It returns ErrClosed if the processor
// has already been shut down.
func (p *Processor) Shutdown(context.Context) error {
	if p == nil {
		panic("logging: Shutdown on nil Processor")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requireOpen("Shutdown")
	if p.closed {
		return ErrClosed
	}
	err := p.f.Close()
	p.f = nil
	p.closed = true
	return errors.Join(p.takeWriteErr(), err)
}

// takeWriteErr returns the latched encode error and clears it. Callers must hold
// p.mu.
func (p *Processor) takeWriteErr() error {
	err := p.writeErr
	p.writeErr = nil
	return err
}
