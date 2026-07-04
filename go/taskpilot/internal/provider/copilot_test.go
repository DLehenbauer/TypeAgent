package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakeCopilotClient struct {
	reply    CopilotReply
	err      error
	lastOpts CopilotOptions
	session  *fakeSession
}

func (f *fakeCopilotClient) Open(ctx context.Context, opts CopilotOptions) (CopilotSession, error) {
	f.lastOpts = opts
	f.session = &fakeSession{reply: f.reply, err: f.err}
	return f.session, nil
}

// fakeSession replays a fixed reply/error for every turn and records the
// prompts it receives so tests can assert on the dialog.
type fakeSession struct {
	reply   CopilotReply
	err     error
	prompts []string
}

func (s *fakeSession) Send(ctx context.Context, prompt string) (CopilotReply, error) {
	s.prompts = append(s.prompts, prompt)
	return s.reply, s.err
}

func (s *fakeSession) Close() {}

// scriptedCopilotClient returns a response computed from the (1-based) Send
// count, allowing tests to model fail-then-succeed transient sequences.
type scriptedCopilotClient struct {
	calls int
	fn    func(call int) (CopilotReply, error)
}

func (s *scriptedCopilotClient) Open(ctx context.Context, opts CopilotOptions) (CopilotSession, error) {
	return &scriptedSession{client: s}, nil
}

type scriptedSession struct{ client *scriptedCopilotClient }

func (s *scriptedSession) Send(ctx context.Context, prompt string) (CopilotReply, error) {
	s.client.calls++
	return s.client.fn(s.client.calls)
}

func (s *scriptedSession) Close() {}

// turnCopilotClient opens a session that returns a scripted reply per turn
// (reusing the final reply once exhausted) and records every prompt, so tests
// can drive and inspect the parse/schema correction dialog.
type turnCopilotClient struct {
	replies []CopilotReply
	session *turnSession
}

func (c *turnCopilotClient) Open(ctx context.Context, opts CopilotOptions) (CopilotSession, error) {
	c.session = &turnSession{replies: c.replies}
	return c.session, nil
}

type turnSession struct {
	replies []CopilotReply
	turn    int
	prompts []string
}

func (s *turnSession) Send(ctx context.Context, prompt string) (CopilotReply, error) {
	s.prompts = append(s.prompts, prompt)
	reply := s.replies[len(s.replies)-1]
	if s.turn < len(s.replies) {
		reply = s.replies[s.turn]
	}
	s.turn++
	return reply, nil
}

func (s *turnSession) Close() {}

func TestCopilotProviderParsesJSONByDefault(t *testing.T) {
	client := &fakeCopilotClient{reply: CopilotReply{Content: `{"answer":42}`, IsAssistant: true}}
	p := NewCopilotProvider(client, 0)

	out, err := p.Submit(context.Background(), Request{Input: map[string]any{
		copilotKeyPrompt: "go",
	}}).Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("output = %#v, want object", out)
	}
	if obj["answer"] != float64(42) {
		t.Fatalf("answer = %v", obj["answer"])
	}
}

func TestCopilotProviderReturnsStringWhenNotJSON(t *testing.T) {
	client := &fakeCopilotClient{reply: CopilotReply{Content: "plain text", IsAssistant: true}}
	p := NewCopilotProvider(client, 0)

	out, err := p.Submit(context.Background(), Request{Input: map[string]any{copilotKeyPrompt: "go"}}).Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out != "plain text" {
		t.Fatalf("output = %v, want plain text", out)
	}
}

func TestCopilotProviderExpectJSONErrorsOnInvalid(t *testing.T) {
	client := &fakeCopilotClient{reply: CopilotReply{Content: "not json", IsAssistant: true}}
	p := NewCopilotProvider(client, 0)

	_, err := p.Submit(context.Background(), Request{Input: map[string]any{
		copilotKeyPrompt:     "go",
		copilotKeyExpectJSON: true,
	}}).Await(context.Background())
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
}

func TestCopilotProviderBuildsPromptWithContextAndSchema(t *testing.T) {
	// The reply is a JSON string, satisfying the {"type":"string"} schema so the
	// structured-validation path accepts it; this test only asserts on the
	// prompt that was built and sent.
	client := &fakeCopilotClient{reply: CopilotReply{Content: `"ok"`, IsAssistant: true}}
	p := NewCopilotProvider(client, 0)

	if _, err := p.Submit(context.Background(), Request{Input: map[string]any{
		copilotKeyPrompt:       "do it",
		copilotKeyContext:      map[string]any{"k": "v"},
		copilotKeyOutputSchema: map[string]any{"type": "string"},
		copilotKeyModel:        "gpt",
	}}).Await(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.session == nil || len(client.session.prompts) == 0 {
		t.Fatal("no prompt was sent to the session")
	}
	sent := client.session.prompts[0]
	if !strings.Contains(sent, "do it") {
		t.Fatalf("prompt missing base text: %q", sent)
	}
	if !strings.Contains(sent, "Context JSON:") {
		t.Fatalf("prompt missing context: %q", sent)
	}
	if !strings.Contains(sent, "output schema") {
		t.Fatalf("prompt missing schema: %q", sent)
	}
	if client.lastOpts.Model != "gpt" {
		t.Fatalf("model = %q", client.lastOpts.Model)
	}
}

func TestCopilotProviderPropagatesError(t *testing.T) {
	client := &fakeCopilotClient{err: errors.New("boom")}
	p := NewCopilotProvider(client, 0)

	_, err := p.Submit(context.Background(), Request{Input: map[string]any{copilotKeyPrompt: "go"}}).Await(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want boom", err)
	}
}

func TestIsTransientCopilotError(t *testing.T) {
	if IsTransientCopilotError(nil) {
		t.Fatal("nil should not be transient")
	}
	if !IsTransientCopilotError(errors.New("429 too many requests")) {
		t.Fatal("rate limit should be transient")
	}
	if !IsTransientCopilotError(context.DeadlineExceeded) {
		t.Fatal("deadline exceeded should be transient")
	}
	if IsTransientCopilotError(errors.New("permanent failure")) {
		t.Fatal("permanent should not be transient")
	}
}

func TestCopilotProviderRetriesTransientErrors(t *testing.T) {
	client := &scriptedCopilotClient{fn: func(call int) (CopilotReply, error) {
		if call == 1 {
			return CopilotReply{}, errors.New("429 too many requests")
		}
		return CopilotReply{Content: "ok", IsAssistant: true}, nil
	}}
	p := NewCopilotProvider(client, 0)

	out, err := p.Submit(context.Background(), Request{Input: map[string]any{
		copilotKeyPrompt:      "go",
		copilotKeyMaxAttempts: 2,
	}}).Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out != "ok" {
		t.Fatalf("out = %v, want ok", out)
	}
	if client.calls != 2 {
		t.Fatalf("calls = %d, want 2 (one retry)", client.calls)
	}
}

func TestCopilotProviderDoesNotRetryPermanentErrors(t *testing.T) {
	client := &scriptedCopilotClient{fn: func(int) (CopilotReply, error) {
		return CopilotReply{}, errors.New("permanent failure")
	}}
	p := NewCopilotProvider(client, 0)

	_, err := p.Submit(context.Background(), Request{Input: map[string]any{
		copilotKeyPrompt:      "go",
		copilotKeyMaxAttempts: 3,
	}}).Await(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if client.calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on permanent error)", client.calls)
	}
}

func TestCopilotProviderStripsCodeFence(t *testing.T) {
	fenced := "```json\n{\"answer\":42}\n```"
	client := &fakeCopilotClient{reply: CopilotReply{Content: fenced, IsAssistant: true}}
	p := NewCopilotProvider(client, 0)

	out, err := p.Submit(context.Background(), Request{Input: map[string]any{
		copilotKeyPrompt:     "go",
		copilotKeyExpectJSON: true,
	}}).Await(context.Background())
	if err != nil {
		t.Fatalf("fenced JSON should parse: %v", err)
	}
	obj, ok := out.(map[string]any)
	if !ok || obj["answer"] != float64(42) {
		t.Fatalf("output = %#v, want {answer:42}", out)
	}
}

func TestCopilotProviderRetriesOnParseErrorWithFeedback(t *testing.T) {
	client := &turnCopilotClient{replies: []CopilotReply{
		{Content: "sorry, here you go: not json", IsAssistant: true},
		{Content: `{"answer":7}`, IsAssistant: true},
	}}
	p := NewCopilotProvider(client, 0)

	out, err := p.Submit(context.Background(), Request{Input: map[string]any{
		copilotKeyPrompt:     "go",
		copilotKeyExpectJSON: true,
	}}).Await(context.Background())
	if err != nil {
		t.Fatalf("dialog should recover: %v", err)
	}
	obj, ok := out.(map[string]any)
	if !ok || obj["answer"] != float64(7) {
		t.Fatalf("output = %#v, want {answer:7}", out)
	}
	if len(client.session.prompts) != 2 {
		t.Fatalf("prompts sent = %d, want 2 (initial + one correction)", len(client.session.prompts))
	}
	if !strings.Contains(client.session.prompts[1], "not valid JSON") {
		t.Fatalf("correction prompt missing parse defect: %q", client.session.prompts[1])
	}
}

func TestCopilotProviderRetriesOnSchemaViolationWithFeedback(t *testing.T) {
	outputSchema := map[string]any{
		"type":     "object",
		"required": []any{"name"},
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
	}
	client := &turnCopilotClient{replies: []CopilotReply{
		{Content: `{"other":1}`, IsAssistant: true},
		{Content: `{"name":"ok"}`, IsAssistant: true},
	}}
	p := NewCopilotProvider(client, 0)

	out, err := p.Submit(context.Background(), Request{Input: map[string]any{
		copilotKeyPrompt:       "go",
		copilotKeyExpectJSON:   true,
		copilotKeyOutputSchema: outputSchema,
	}}).Await(context.Background())
	if err != nil {
		t.Fatalf("dialog should recover: %v", err)
	}
	obj, ok := out.(map[string]any)
	if !ok || obj["name"] != "ok" {
		t.Fatalf("output = %#v, want {name:ok}", out)
	}
	if len(client.session.prompts) != 2 {
		t.Fatalf("prompts sent = %d, want 2", len(client.session.prompts))
	}
	if !strings.Contains(client.session.prompts[1], "output schema") {
		t.Fatalf("correction prompt missing schema defect: %q", client.session.prompts[1])
	}
}

func TestCopilotProviderFailsAfterValidationAttemptsExhausted(t *testing.T) {
	client := &turnCopilotClient{replies: []CopilotReply{
		{Content: "still not json", IsAssistant: true},
	}}
	p := NewCopilotProvider(client, 0)

	const budget = 2

	_, err := p.Submit(context.Background(), Request{Input: map[string]any{
		copilotKeyPrompt:                "go",
		copilotKeyExpectJSON:            true,
		copilotKeyMaxValidationAttempts: budget,
	}}).Await(context.Background())
	if err == nil {
		t.Fatal("expected validation failure")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("after %d attempt", budget)) {
		t.Fatalf("err = %v, want it to mention the attempt budget", err)
	}
	if len(client.session.prompts) != budget {
		t.Fatalf("prompts sent = %d, want %d (the budget)", len(client.session.prompts), budget)
	}
}

func TestParseCopilotConfigDefaults(t *testing.T) {
	cfg, err := parseCopilotConfig(map[string]any{copilotKeyPrompt: "go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Policy != defaultCopilotPolicy() {
		t.Fatalf("policy = %#v, want default %#v", cfg.Policy, defaultCopilotPolicy())
	}
	if cfg.ValidationAttempts != defaultValidationAttempts {
		t.Fatalf("validation attempts = %d, want default %d", cfg.ValidationAttempts, defaultValidationAttempts)
	}
	if cfg.SessionIdleTimeout != 0 {
		t.Fatalf("idle timeout = %d, want 0 (unbounded)", cfg.SessionIdleTimeout)
	}
}

func TestParseCopilotConfigOverrides(t *testing.T) {
	cfg, err := parseCopilotConfig(map[string]any{
		copilotKeyMaxValidationAttempts:     4,
		copilotKeySessionIdleTimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ValidationAttempts != 4 {
		t.Fatalf("validation attempts = %d, want 4", cfg.ValidationAttempts)
	}
	if cfg.SessionIdleTimeout != 30 {
		t.Fatalf("idle timeout = %d, want 30", cfg.SessionIdleTimeout)
	}
}

func TestParseCopilotConfigRejectsInvalidKnobs(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
	}{
		{"non-positive maxValidationAttempts", map[string]any{copilotKeyMaxValidationAttempts: 0}},
		{"negative maxValidationAttempts", map[string]any{copilotKeyMaxValidationAttempts: -2}},
		{"non-integer maxValidationAttempts", map[string]any{copilotKeyMaxValidationAttempts: "several"}},
		{"non-positive sessionIdleTimeoutSeconds", map[string]any{copilotKeySessionIdleTimeoutSeconds: 0}},
		{"negative sessionIdleTimeoutSeconds", map[string]any{copilotKeySessionIdleTimeoutSeconds: -5}},
		{"fractional sessionIdleTimeoutSeconds", map[string]any{copilotKeySessionIdleTimeoutSeconds: 1.5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseCopilotConfig(tc.in); err == nil {
				t.Fatalf("parseCopilotConfig(%v) = nil error, want rejection", tc.in)
			}
			// The same boundary check backs dry-run planning via CopilotPolicy.
			if _, err := CopilotPolicy(tc.in); err == nil {
				t.Fatalf("CopilotPolicy(%v) = nil error, want rejection", tc.in)
			}
		})
	}
}

func TestCopilotProviderRejectsInvalidOverrideBeforeInvoking(t *testing.T) {
	client := &turnCopilotClient{replies: []CopilotReply{{Content: `{"answer":1}`, IsAssistant: true}}}
	p := NewCopilotProvider(client, 0)

	_, err := p.Submit(context.Background(), Request{Input: map[string]any{
		copilotKeyPrompt:                "go",
		copilotKeyMaxValidationAttempts: 0,
	}}).Await(context.Background())
	if err == nil {
		t.Fatal("expected an invalid maxValidationAttempts to be rejected at the boundary")
	}
	if client.session != nil {
		t.Fatal("no session should be opened when an override is invalid")
	}
}
