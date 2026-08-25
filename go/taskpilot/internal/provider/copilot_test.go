package provider

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
)

type fakeSDKRuntime struct {
	mu            sync.Mutex
	running       bool
	startCalls    int
	processStarts int
	sessionCalls  int
}

func (f *fakeSDKRuntime) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if !f.running {
		f.running = true
		f.processStarts++
	}
	return nil
}

func (f *fakeSDKRuntime) CreateSession(ctx context.Context, _ *copilot.SessionConfig) (*copilot.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !f.running {
		return nil, errors.New("CLI process exited: EOF")
	}
	f.sessionCalls++
	return nil, nil
}

func (f *fakeSDKRuntime) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
	return nil
}

func (f *fakeSDKRuntime) crash() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
}

func (f *fakeSDKRuntime) counts() (startCalls, processStarts, sessionCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCalls, f.processStarts, f.sessionCalls
}

func newFakeSDKClient(runtime sdkRuntime) *sdkClient {
	return &sdkClient{newClient: func() sdkRuntime { return runtime }}
}

func TestSDKClientRestartsExitedProcess(t *testing.T) {
	runtime := &fakeSDKRuntime{}
	client := newFakeSDKClient(runtime)

	first, err := client.Open(context.Background(), CopilotOptions{})
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	first.Close()

	runtime.crash()

	second, err := client.Open(context.Background(), CopilotOptions{})
	if err != nil {
		t.Fatalf("Open() after crash error = %v", err)
	}
	second.Close()

	startCalls, processStarts, sessionCalls := runtime.counts()
	if startCalls != 2 {
		t.Fatalf("Start calls = %d, want 2 (one lifecycle check per Open)", startCalls)
	}
	if processStarts != 2 {
		t.Fatalf("process starts = %d, want 2 (initial start plus restart)", processStarts)
	}
	if sessionCalls != 2 {
		t.Fatalf("session calls = %d, want 2", sessionCalls)
	}
}

func TestSDKClientConcurrentOpenAfterCrashSharesRestart(t *testing.T) {
	runtime := &fakeSDKRuntime{}
	client := newFakeSDKClient(runtime)

	session, err := client.Open(context.Background(), CopilotOptions{})
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	session.Close()
	runtime.crash()

	const opens = 8
	errs := make(chan error, opens)
	var wg sync.WaitGroup
	for range opens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, err := client.Open(context.Background(), CopilotOptions{})
			if err == nil {
				session.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Open() error = %v", err)
		}
	}

	_, processStarts, sessionCalls := runtime.counts()
	if processStarts != 2 {
		t.Fatalf("process starts = %d, want 2 (one shared restart)", processStarts)
	}
	if sessionCalls != opens+1 {
		t.Fatalf("session calls = %d, want %d", sessionCalls, opens+1)
	}
}

func TestSDKClientCanceledRestartDoesNotPoisonLaterOpen(t *testing.T) {
	runtime := &fakeSDKRuntime{}
	client := newFakeSDKClient(runtime)

	session, err := client.Open(context.Background(), CopilotOptions{})
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	session.Close()
	runtime.crash()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Open(ctx, CopilotOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open() with canceled context error = %v, want context.Canceled", err)
	}

	session, err = client.Open(context.Background(), CopilotOptions{})
	if err != nil {
		t.Fatalf("Open() after canceled restart error = %v", err)
	}
	session.Close()

	_, processStarts, sessionCalls := runtime.counts()
	if processStarts != 2 {
		t.Fatalf("process starts = %d, want 2", processStarts)
	}
	if sessionCalls != 2 {
		t.Fatalf("session calls = %d, want 2", sessionCalls)
	}
}

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

func TestCopilotProviderRejectsNonAssistantStructuredReply(t *testing.T) {
	client := &fakeCopilotClient{reply: CopilotReply{Content: `{"answer":42}`}}
	p := NewCopilotProvider(client, 0)

	_, err := p.Submit(context.Background(), Request{Input: map[string]any{
		copilotKeyPrompt:     "go",
		copilotKeyExpectJSON: true,
	}}).Await(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires an assistant reply") {
		t.Fatalf("error = %v, want structured assistant-reply error", err)
	}
	if got := len(client.session.prompts); got != 1 {
		t.Fatalf("send count = %d, want 1", got)
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

func TestCopilotProviderDoesNotReplayTimedOutTurn(t *testing.T) {
	client := &scriptedCopilotClient{fn: func(call int) (CopilotReply, error) {
		return CopilotReply{}, context.DeadlineExceeded
	}}
	p := NewCopilotProvider(client, 0)

	_, err := p.Submit(context.Background(), Request{Input: map[string]any{
		copilotKeyPrompt:      "go",
		copilotKeyMaxAttempts: 3,
	}}).Await(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if client.calls != 1 {
		t.Fatalf("send count = %d, want 1", client.calls)
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
		t.Fatalf("idle timeout = %s, want 0 (unbounded)", cfg.SessionIdleTimeout)
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
	if cfg.SessionIdleTimeout != 30*time.Second {
		t.Fatalf("idle timeout = %s, want 30s", cfg.SessionIdleTimeout)
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

func TestParseCopilotConfigRejectsIdleTimeoutOverflow(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("int cannot represent overflowing duration seconds")
	}
	const maxDuration = time.Duration(1<<63 - 1)
	tooLarge := int(maxDuration/time.Second) + 1
	if _, err := parseCopilotConfig(map[string]any{
		copilotKeySessionIdleTimeoutSeconds: tooLarge,
	}); err == nil {
		t.Fatal("expected idle timeout overflow error")
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
