package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/retry"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/schema"
	copilot "github.com/github/copilot-sdk/go"
)

// Copilot input keys. These name the fields of the copilot.invoke task schema
// (copilotInvokeInput in internal/builtin) as the provider sees them in the
// decoded input map. Declaring them once here keeps the provider's lookups from
// drifting from the schema's json tags. Keep this set in sync with the struct.
const (
	copilotKeyPrompt                    = "prompt"
	copilotKeyContext                   = "context"
	copilotKeyOutputSchema              = "outputSchema"
	copilotKeyModel                     = "model"
	copilotKeyReasoningEffort           = "reasoningEffort"
	copilotKeyContextTier               = "contextTier"
	copilotKeyWorkingDirectory          = "workingDirectory"
	copilotKeyPermissionMode            = "permissionMode"
	copilotKeyExpectJSON                = "expectJson"
	copilotKeySessionIdleTimeoutSeconds = "sessionIdleTimeoutSeconds"
	copilotKeyMaxAttempts               = "maxAttempts"
	copilotKeyInitialBackoffSeconds     = "initialBackoffSeconds"
	copilotKeyMaxBackoffSeconds         = "maxBackoffSeconds"
	copilotKeyMaxValidationAttempts     = "maxValidationAttempts"
)

// defaultCopilotPolicy is the conservative transient-retry policy applied when
// the node does not override the maxAttempts/backoff knobs. It is built fresh on
// each call so callers cannot mutate a shared package-level default.
func defaultCopilotPolicy() retry.Policy {
	return retry.Policy{
		MaxAttempts:    5,
		InitialBackoff: time.Second,
		MaxBackoff:     2 * time.Minute,
	}
}

// CopilotOptions captures the session-level inputs for a Copilot conversation.
// Per-turn prompts are passed to CopilotSession.Send, not carried here. Fields
// that the SDK types explicitly (e.g. ContextTier) reference the SDK types
// directly so they cannot drift from the upstream definitions.
type CopilotOptions struct {
	Model                     string
	ReasoningEffort           string
	WorkingDirectory          string
	PermissionMode            string
	ContextTier               copilot.ContextTier
	SessionIdleTimeoutSeconds int
}

// CopilotReply is the raw result of a single Copilot turn.
type CopilotReply struct {
	Content     string
	IsAssistant bool
}

// CopilotSession is a live, multi-turn Copilot conversation. Each Send issues
// one prompt and waits for the reply; successive Sends reuse the same session
// so the model retains the prior turns as context (this is how the provider
// feeds parse/schema errors back for correction). Close disconnects the
// session without tearing down the client that opened it. A session is not
// required to be safe for concurrent use; the provider drives one turn at a
// time.
type CopilotSession interface {
	Send(ctx context.Context, prompt string) (CopilotReply, error)
	Close()
}

// CopilotClient opens a Copilot session. It abstracts the SDK so
// CopilotProvider can be unit-tested without the SDK or network.
type CopilotClient interface {
	Open(ctx context.Context, opts CopilotOptions) (CopilotSession, error)
}

// sdkClient is the default CopilotClient backed by the Copilot SDK. A single
// underlying copilot.Client -- i.e. a single CLI server process -- is shared
// across every invocation; each Open creates a session on that shared client
// and the returned session's Close disconnects only that session. Spawning one
// process per invocation is what previously drove the machine to OOM, so the
// process is started lazily on first use and reused thereafter. Close stops the
// shared client and its process.
type sdkClient struct {
	mu      sync.Mutex
	client  *copilot.Client
	started bool
}

// newSDKClient returns an sdkClient with no process running yet; the shared
// client is started on the first Open.
func newSDKClient() *sdkClient { return &sdkClient{} }

// ensureClient starts the shared CLI server process exactly once and returns
// it. Concurrent first-callers serialize on the mutex so only one process is
// spawned; a failed start leaves the client unstarted so a later Open (or the
// provider's transient retry) can try again.
func (c *sdkClient) ensureClient(ctx context.Context) (*copilot.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return c.client, nil
	}
	client := copilot.NewClient(&copilot.ClientOptions{})
	if err := client.Start(ctx); err != nil {
		return nil, err
	}
	c.client = client
	c.started = true
	return client, nil
}

// Open creates a session on the shared client from opts and returns it as a
// CopilotSession. The caller owns the session and must Close it; closing a
// session disconnects it without stopping the shared client. WorkingDirectory
// is applied per session so invocations with different directories can share
// one process.
func (c *sdkClient) Open(ctx context.Context, opts CopilotOptions) (CopilotSession, error) {
	client, err := c.ensureClient(ctx)
	if err != nil {
		return nil, err
	}

	sessionConfig := &copilot.SessionConfig{}
	if opts.Model != "" {
		sessionConfig.Model = opts.Model
	}
	if opts.ReasoningEffort != "" {
		sessionConfig.ReasoningEffort = opts.ReasoningEffort
	}
	if opts.ContextTier != "" {
		sessionConfig.ContextTier = opts.ContextTier
	}
	if opts.WorkingDirectory != "" {
		sessionConfig.WorkingDirectory = opts.WorkingDirectory
	}
	if opts.PermissionMode == "" || opts.PermissionMode == "approveAll" {
		sessionConfig.OnPermissionRequest = copilot.PermissionHandler.ApproveAll
	}
	session, err := client.CreateSession(ctx, sessionConfig)
	if err != nil {
		return nil, err
	}
	return &sdkSession{session: session, idleTimeout: opts.SessionIdleTimeoutSeconds}, nil
}

// Close stops the shared client and its CLI server process. It is safe to call
// once at provider teardown; a client that was never started is a no-op.
func (c *sdkClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		c.client.Stop()
		c.client = nil
		c.started = false
	}
}

// sdkSession is the SDK-backed CopilotSession returned by sdkClient.Open. It
// wraps a single session on the shared client; Close disconnects only this
// session and leaves the shared client running for other sessions.
type sdkSession struct {
	session     *copilot.Session
	idleTimeout int
}

// Send issues one prompt on the session and waits for the assistant reply,
// bounding the wait by the configured idle timeout when set. A nil reply yields
// an empty CopilotReply; a non-assistant reply is returned with IsAssistant
// false so the provider can pass it through untouched.
func (s *sdkSession) Send(ctx context.Context, prompt string) (CopilotReply, error) {
	sendCtx := ctx
	if s.idleTimeout > 0 {
		var cancel context.CancelFunc
		sendCtx, cancel = context.WithTimeout(ctx, time.Duration(s.idleTimeout)*time.Second)
		defer cancel()
	}
	reply, err := s.session.SendAndWait(sendCtx, copilot.MessageOptions{Prompt: prompt})
	if err != nil {
		return CopilotReply{}, err
	}
	if reply == nil {
		return CopilotReply{}, nil
	}
	if data, ok := reply.Data.(*copilot.AssistantMessageData); ok {
		return CopilotReply{Content: data.Content, IsAssistant: true}, nil
	}
	return CopilotReply{Content: fmt.Sprint(reply.Data)}, nil
}

// Close disconnects this session, releasing its in-memory resources while
// leaving the shared client running for other sessions. It is safe to call
// once; the error is intentionally ignored because there is no recovery at
// teardown.
func (s *sdkSession) Close() {
	if s.session != nil {
		_ = s.session.Disconnect()
	}
}

// CopilotProvider invokes Copilot to produce structured output.
type CopilotProvider struct {
	client   CopilotClient
	throttle *throttle
}

// NewCopilotProvider returns a CopilotProvider. A nil client uses the default
// SDK-backed client. limit bounds concurrent invocations; a non-positive limit
// runs invocations unbounded.
func NewCopilotProvider(client CopilotClient, limit int) *CopilotProvider {
	if client == nil {
		client = newSDKClient()
	}
	return &CopilotProvider{client: client, throttle: newThrottle(limit)}
}

// Name reports the provider's identity, NameCopilot.
func (p *CopilotProvider) Name() Name { return NameCopilot }

// Close releases the provider's shared Copilot client, stopping the CLI server
// process it spawned. It is a no-op when the underlying client does not manage
// a process (e.g. a test fake). Safe to call once at provider teardown.
func (p *CopilotProvider) Close() {
	if c, ok := p.client.(interface{ Close() }); ok {
		c.Close()
	}
}

// copilotConfig is the fully validated set of per-invocation pacing knobs parsed
// once from the node input at the boundary: the transient-retry policy, the
// validation-dialog turn budget, and the session idle timeout in seconds (0 =
// unbounded). Parsing rejects any illegal override rather than silently
// defaulting or clamping it later.
type copilotConfig struct {
	Policy             retry.Policy
	ValidationAttempts int
	SessionIdleTimeout int
}

// parseCopilotConfig validates every per-invocation knob the input carries and
// returns them together. It surfaces the first invalid override as an error so
// an illegal retry policy, validation budget, or idle timeout is rejected up
// front instead of being silently defaulted or clamped deeper in the run.
func parseCopilotConfig(input map[string]any) (copilotConfig, error) {
	policy, err := policyFromInput(input, defaultCopilotPolicy())
	if err != nil {
		return copilotConfig{}, err
	}
	attempts, err := validationAttempts(input)
	if err != nil {
		return copilotConfig{}, err
	}
	idle, err := sessionIdleTimeout(input)
	if err != nil {
		return copilotConfig{}, err
	}
	return copilotConfig{Policy: policy, ValidationAttempts: attempts, SessionIdleTimeout: idle}, nil
}

// CopilotPolicy returns the transient-retry policy that would be applied to a
// Copilot invocation with the given input, after validating every per-
// invocation override the input carries (retry pacing, validation-attempt
// budget, and session idle timeout). It is exposed so callers such as dry-run
// planning can surface the effective policy and reject an invalid override
// without invoking Copilot.
func CopilotPolicy(input map[string]any) (retry.Policy, error) {
	cfg, err := parseCopilotConfig(input)
	if err != nil {
		return retry.Policy{}, err
	}
	return cfg.Policy, nil
}

// Submit dispatches the request asynchronously through the throttle and returns
// a Future that resolves once the Copilot invocation completes.
func (p *CopilotProvider) Submit(ctx context.Context, req Request) Future {
	return p.throttle.dispatch(ctx, func(ctx context.Context) (Result, error) {
		return p.run(ctx, req)
	})
}

func (p *CopilotProvider) run(ctx context.Context, req Request) (Result, error) {
	input := req.Input
	cfg, err := parseCopilotConfig(input)
	if err != nil {
		return nil, err
	}
	prompt, err := buildPrompt(input)
	if err != nil {
		return nil, err
	}
	tier, err := parseContextTier(asString(input[copilotKeyContextTier]))
	if err != nil {
		return nil, err
	}
	opts := CopilotOptions{
		Model:                     asString(input[copilotKeyModel]),
		ReasoningEffort:           asString(input[copilotKeyReasoningEffort]),
		WorkingDirectory:          asString(input[copilotKeyWorkingDirectory]),
		PermissionMode:            asString(input[copilotKeyPermissionMode]),
		ContextTier:               tier,
		SessionIdleTimeoutSeconds: cfg.SessionIdleTimeout,
	}

	// One transient-retry configuration drives both session open and every
	// turn so they replay the same way; divergence would observably change
	// retry behavior between session open and turn.
	retryOpts := retry.Options{
		Policy:  cfg.Policy,
		OnError: func(err error, _ retry.Attempt) bool { return IsTransientCopilotError(err) },
	}

	session, err := p.openSession(ctx, opts, retryOpts)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	// Two retry layers cooperate here and stay strictly separate:
	//   - Transient retry (openSession/sendTurn): re-issues a network call when
	//     Copilot throttles or the connection blips. Owned by the provider
	//     because only it knows which SDK failures are safe to replay.
	//   - Validation dialog (this loop): when the reply does not parse as JSON
	//     or violates the output schema, that is not an error -- it is part of
	//     the conversation. We send the specific defect back on the SAME session
	//     and ask the model to correct itself, bounded by maxValidationAttempts.
	expectJSON, _ := input[copilotKeyExpectJSON].(bool)
	outputSchema := input[copilotKeyOutputSchema]
	wantStructured := expectJSON || outputSchema != nil
	maxTurns := cfg.ValidationAttempts

	turnPrompt := prompt
	var lastErr error
	for attempt := 1; attempt <= maxTurns; attempt++ {
		reply, err := p.sendTurn(ctx, session, turnPrompt, retryOpts)
		if err != nil {
			return nil, err
		}
		if !reply.IsAssistant {
			return reply.Content, nil
		}
		if !wantStructured {
			// No structured contract requested: opportunistically parse JSON
			// (tolerating a code fence) and fall back to the raw string.
			if value, ok := tryParseJSON(reply.Content); ok {
				return value, nil
			}
			return reply.Content, nil
		}
		value, verr := parseStructured(reply.Content, outputSchema)
		if verr == nil {
			return value, nil
		}
		// Feed the precise parse/schema defect back for the next turn.
		lastErr = verr
		turnPrompt = correctionPrompt(verr)
	}
	return nil, fmt.Errorf("copilot reply failed validation after %d attempt(s): %w", maxTurns, lastErr)
}

// openSession opens a Copilot session, replaying the open through the provided
// transient-error retry options so throttling or connection blips during
// startup do not surface as task failures.
func (p *CopilotProvider) openSession(ctx context.Context, opts CopilotOptions, retryOpts retry.Options) (CopilotSession, error) {
	out, err := retry.Run(ctx, retryOpts, func(ctx context.Context) (any, error) {
		return p.client.Open(ctx, opts)
	})
	if err != nil {
		return nil, err
	}
	session, ok := out.(CopilotSession)
	if !ok {
		return nil, fmt.Errorf("copilot: unexpected session result type %T", out)
	}
	return session, nil
}

// sendTurn issues one conversation turn, replaying only transient failures.
// Parse/schema handling is the caller's concern, not a transient error.
func (p *CopilotProvider) sendTurn(ctx context.Context, session CopilotSession, prompt string, retryOpts retry.Options) (CopilotReply, error) {
	out, err := retry.Run(ctx, retryOpts, func(ctx context.Context) (any, error) {
		return session.Send(ctx, prompt)
	})
	if err != nil {
		return CopilotReply{}, err
	}
	return out.(CopilotReply), nil
}

// parseContextTier validates the config-supplied context tier and returns the
// typed value. An empty string leaves the tier unset (the model's default).
func parseContextTier(s string) (copilot.ContextTier, error) {
	switch copilot.ContextTier(s) {
	case "":
		return "", nil
	case copilot.ContextTierDefault, copilot.ContextTierLongContext:
		return copilot.ContextTier(s), nil
	default:
		return "", fmt.Errorf("invalid contextTier %q: want %q or %q", s, copilot.ContextTierDefault, copilot.ContextTierLongContext)
	}
}

func buildPrompt(input map[string]any) (string, error) {
	var b strings.Builder
	b.WriteString(asString(input[copilotKeyPrompt]))
	if ctx, ok := input[copilotKeyContext]; ok && ctx != nil {
		ctx, files := replaceFileRefs(ctx)
		raw, err := json.MarshalIndent(ctx, "", "  ")
		if err != nil {
			return "", err
		}
		b.WriteString("\n\nContext JSON:\n")
		b.Write(raw)
		if len(files) > 0 {
			b.WriteString("\n\nThe files referenced by path above live on disk. Read them with your tools (reading only the parts you need) instead of expecting their contents inline:\n")
			for _, f := range files {
				b.WriteString("- ")
				b.WriteString(f)
				b.WriteString("\n")
			}
		}
	}
	if outSchema, ok := input[copilotKeyOutputSchema]; ok && outSchema != nil {
		raw, err := json.MarshalIndent(outSchema, "", "  ")
		if err != nil {
			return "", err
		}
		b.WriteString("\n\nReturn only JSON matching this output schema:\n```json\n")
		b.Write(raw)
		b.WriteString("\n```\n")
	}
	return b.String(), nil
}

// defaultValidationAttempts bounds the parse/schema correction dialog when the
// node does not set maxValidationAttempts. One initial reply plus a couple of
// correction turns is enough to recover from the common failure (a stray code
// fence or a missing field) without burning an unbounded number of sessions.
const defaultValidationAttempts = 3

// validationAttempts returns the maximum number of conversation turns allowed
// while coaxing a parseable, schema-valid reply out of the model, honoring the
// node's maxValidationAttempts override when set. A present-but-illegal
// override is rejected rather than silently falling back to the default.
func validationAttempts(input map[string]any) (int, error) {
	v, ok, err := positiveOverride(input, copilotKeyMaxValidationAttempts)
	if err != nil {
		return 0, err
	}
	if ok {
		return v, nil
	}
	return defaultValidationAttempts, nil
}

// sessionIdleTimeout returns the per-invocation session idle timeout in seconds,
// or 0 (unbounded) when the node does not set sessionIdleTimeoutSeconds. A
// present-but-illegal value is rejected rather than being silently clamped to
// the unbounded default.
func sessionIdleTimeout(input map[string]any) (int, error) {
	v, ok, err := positiveOverride(input, copilotKeySessionIdleTimeoutSeconds)
	if err != nil {
		return 0, err
	}
	if ok {
		return v, nil
	}
	return 0, nil
}

// extractJSON returns the JSON payload from a model reply, stripping a
// surrounding Markdown code fence (``` or ```json ... ```) when present.
// Models frequently fence structured output despite being asked not to; this
// keeps that habit from failing the parse. When no fence is found the trimmed
// input is returned unchanged.
func extractJSON(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	// Drop the opening fence line, which may carry a language tag (```json).
	if nl := strings.IndexByte(t, '\n'); nl >= 0 {
		t = t[nl+1:]
	} else {
		t = ""
	}
	// Drop the closing fence and any trailing prose after it.
	if end := strings.LastIndex(t, "```"); end >= 0 {
		t = t[:end]
	}
	return strings.TrimSpace(t)
}

// tryParseJSON parses content (tolerating a code fence) as JSON, reporting
// whether it succeeded. It is used on the unstructured path where a JSON reply
// is welcome but a plain-string reply is equally valid.
func tryParseJSON(content string) (any, bool) {
	var value any
	if err := json.Unmarshal([]byte(extractJSON(content)), &value); err != nil {
		return nil, false
	}
	return value, true
}

// parseStructured parses content as JSON (tolerating a code fence) and, when
// outputSchema is non-nil, validates the result against it. The returned error
// is phrased for the model: it names the defect (invalid JSON or the specific
// schema violation) so correctionPrompt can hand it back verbatim.
func parseStructured(content string, outputSchema any) (any, error) {
	var value any
	if err := json.Unmarshal([]byte(extractJSON(content)), &value); err != nil {
		return nil, fmt.Errorf("the reply was not valid JSON: %v", err)
	}
	if outputSchema != nil {
		if err := schema.Validate(outputSchema, value); err != nil {
			return nil, fmt.Errorf("the JSON did not match the required output schema: %v", err)
		}
	}
	return value, nil
}

// correctionPrompt turns a parse/schema defect into the next conversation turn,
// instructing the model to resend corrected JSON with no fence or prose.
func correctionPrompt(err error) string {
	return fmt.Sprintf("Your previous reply was rejected: %s\n\n"+
		"Reply again with ONLY the corrected JSON value. Do not include any "+
		"explanation, commentary, or Markdown code fences.", err)
}

// IsTransientCopilotError reports whether err is a transient Copilot failure
// that warrants a retry.
func IsTransientCopilotError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	transientFragments := []string{
		"waiting for session.idle",
		"context deadline exceeded",
		"timeout",
		"timed out",
		"rate limit",
		"rate-limit",
		"too many requests",
		"429",
		"temporarily unavailable",
		"temporary",
		"connection reset",
		"connection refused",
		"eof",
		"broken pipe",
	}
	for _, fragment := range transientFragments {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}
