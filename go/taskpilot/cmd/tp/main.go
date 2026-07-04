package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/builtin"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/cache"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/engine"
	tflog "github.com/microsoft/TypeAgent/go/taskpilot/internal/logging"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/parser"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/schema"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/telemetry"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/verify"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/version"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "tp:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printHelp(stdout)
		return nil
	}

	cmd := args[0]
	if cmd == "-v" || cmd == "--version" || cmd == "version" {
		fmt.Fprintln(stdout, version.Version())
		return nil
	}

	if parser.FormatForPath(cmd) != parser.FormatUnknown {
		args = append([]string{"run"}, args...)
		cmd = "run"
	}

	for _, c := range commands {
		if c.name == cmd {
			return c.run(args[1:], stdout, stderr)
		}
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// command is a single top-level subcommand. The commands table is the sole
// source of truth for both dispatch (run) and help output (printHelp), so
// adding or renaming a command is a one-line edit.
type command struct {
	name string
	// usage returns the help line(s) for this command. It is a function so
	// dynamic usage (e.g. per-provider --NAME-parallel flags) can be computed.
	usage func() string
	run   func(args []string, stdout, stderr io.Writer) error
}

var commands = []command{
	{
		name: "run",
		usage: func() string {
			var parallel strings.Builder
			for _, name := range provider.Names() {
				fmt.Fprintf(&parallel, " [--%s-parallel N]", name)
			}
			return fmt.Sprintf("  tp run <root-task-file> [--input file.json|-.] [--dry-run] [--quiet] [--max-parallel N]%s [--otlp-endpoint host:port] [--key value...]", parallel.String())
		},
		run: func(args []string, stdout, stderr io.Writer) error { return runWorkflow(args, stdout, stderr) },
	},
	{
		name:  "verify",
		usage: func() string { return "  tp verify <root-task-file>" },
		run:   func(args []string, stdout, stderr io.Writer) error { return verifyWorkflow(args, stdout) },
	},
	{
		name:  "graph",
		usage: func() string { return "  tp graph <root-task-file> --format dot|json" },
		run:   func(args []string, stdout, stderr io.Writer) error { return graphWorkflow(args, stdout) },
	},
	{
		name:  "log",
		usage: func() string { return "  tp log <run-id> [--json] [--tail]" },
		run:   func(args []string, stdout, stderr io.Writer) error { return logCommand(args, stdout) },
	},
	{
		name:  "cache",
		usage: func() string { return "  tp cache gc" },
		run:   func(args []string, stdout, stderr io.Writer) error { return cacheCommand(args, stdout) },
	},
	{
		name:  "paths",
		usage: func() string { return "  tp paths" },
		run:   func(args []string, stdout, stderr io.Writer) error { return pathsCommand(args, stdout) },
	},
	{
		name:  "builtin",
		usage: func() string { return "  tp builtin list" },
		run:   func(args []string, stdout, stderr io.Writer) error { return builtinCommand(args, stdout) },
	},
}

// defaultMaxParallel is a generous engine-wide safety ceiling on how many graph
// nodes run at once. Per-provider concurrency caps are the real throttle for
// side-effecting work, so this is sized to let cheap pure builtins fan out
// freely rather than to gate providers.
const defaultMaxParallel = 64

func printHelp(w io.Writer) {
	fmt.Fprint(w, `tp - local JSON/JSONL workflow runner (taskpilot)

Usage:
`)
	for _, c := range commands {
		fmt.Fprintln(w, c.usage())
	}
}

type runOptions struct {
	file         string
	inputFile    string
	dryRun       bool
	quiet        bool
	maxParallel  int
	limits       provider.Limits
	otlpEndpoint string
	dynamic      map[string]string
	jsonValues   map[string]any
}

func parseRunArgs(args []string) (runOptions, error) {
	opts := runOptions{
		maxParallel: defaultMaxParallel,
		limits:      provider.DefaultLimits(),
		dynamic:     map[string]string{},
		jsonValues:  map[string]any{},
	}
	if len(args) == 0 {
		return opts, errors.New("missing root task file")
	}
	opts.file = args[0]
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--input":
			i++
			if i >= len(args) {
				return opts, errors.New("--input requires a value")
			}
			opts.inputFile = args[i]
		case "--dry-run", "-WhatIf", "--WhatIf":
			opts.dryRun = true
		case "--quiet":
			opts.quiet = true
		case "--max-parallel":
			i++
			if i >= len(args) {
				return opts, errors.New("--max-parallel requires a value")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				return opts, fmt.Errorf("invalid --max-parallel value %q", args[i])
			}
			opts.maxParallel = n
		case "--otlp-endpoint":
			i++
			if i >= len(args) {
				return opts, errors.New("--otlp-endpoint requires a value")
			}
			opts.otlpEndpoint = args[i]
		case "--json":
			i++
			if i >= len(args) {
				return opts, errors.New("--json requires key=value")
			}
			key, value, ok := strings.Cut(args[i], "=")
			if !ok || key == "" {
				return opts, fmt.Errorf("--json expects key=value, got %q", args[i])
			}
			var parsed any
			if err := json.Unmarshal([]byte(value), &parsed); err != nil {
				return opts, fmt.Errorf("--json %s value must be JSON: %w", key, err)
			}
			opts.jsonValues[key] = parsed
		default:
			if name, ok := providerParallelFlag(arg, opts.limits); ok {
				i++
				if i >= len(args) {
					return opts, fmt.Errorf("%s requires a value", arg)
				}
				n, err := strconv.Atoi(args[i])
				if err != nil || n < 1 {
					return opts, fmt.Errorf("invalid %s value %q", arg, args[i])
				}
				opts.limits[name] = n
				continue
			}
			if !strings.HasPrefix(arg, "--") {
				return opts, fmt.Errorf("unexpected argument %q", arg)
			}
			key := strings.TrimPrefix(arg, "--")
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s requires a value", arg)
			}
			opts.dynamic[key] = args[i]
		}
	}
	return opts, nil
}

// providerParallelFlag reports whether arg is the per-provider concurrency flag
// (--<name>-parallel) for a provider in the current roster, returning that
// provider's name. The roster is the sole source of valid provider names, so
// the CLI never mirrors the built-in provider list.
func providerParallelFlag(arg string, roster provider.Limits) (provider.Name, bool) {
	trimmed, ok := strings.CutPrefix(arg, "--")
	if !ok {
		return "", false
	}
	base, ok := strings.CutSuffix(trimmed, "-parallel")
	if !ok {
		return "", false
	}
	name := provider.Name(base)
	if _, exists := roster[name]; !exists {
		return "", false
	}
	return name, true
}

func loadVerified(file string) (*verify.VerifiedDocument, error) {
	doc, err := parser.LoadFile(file)
	if err != nil {
		return nil, err
	}
	reg := builtin.SchemaRegistry()
	return verify.Document(doc, reg)
}

// runLocations holds the on-disk artifacts for a run so the up-front header and
// both the success and failure footers print the same set of paths from one
// place instead of duplicating (and drifting) the list.
type runLocations struct {
	workflow   string
	logPath    string
	outputPath string
	cacheDir   string
	otlpLabel  string // empty when OTLP export is disabled
}

// writePaths prints the artifact locations shared by every run summary. The
// output file only exists on success, so the failure footer passes
// includeOutput=false to omit a path that was never written.
func (l runLocations) writePaths(w io.Writer, includeOutput bool) {
	fmt.Fprintf(w, "  workflow: %s\n", l.workflow)
	fmt.Fprintf(w, "  log:      %s\n", l.logPath)
	if includeOutput {
		fmt.Fprintf(w, "  output:   %s\n", l.outputPath)
	}
	fmt.Fprintf(w, "  cache:    %s\n", l.cacheDir)
	if l.otlpLabel != "" {
		fmt.Fprintf(w, "  otlp:     %s\n", l.otlpLabel)
	}
}

func runWorkflow(args []string, stdout, stderr io.Writer) error {
	opts, err := parseRunArgs(args)
	if err != nil {
		return err
	}
	res, err := loadVerified(opts.file)
	if err != nil {
		return err
	}
	input, err := rootInput(res, opts)
	if err != nil {
		return err
	}

	state, err := cache.ResolveStateDir()
	if err != nil {
		return err
	}
	cacheDir := cache.CacheDir(state)
	store := cache.New(cacheDir)
	if err := store.Init(); err != nil {
		return err
	}
	runID := engine.NewRunID()
	logDir := filepath.Join(state, "logs")
	logPath := tflog.LogPath(logDir, runID)
	outputPath := filepath.Join(logDir, runID+".output.json")
	proc, err := tflog.Open(logDir, runID)
	if err != nil {
		return err
	}
	otlpProc, otlpLabel, err := otlpProcessor(context.Background(), opts.otlpEndpoint)
	if err != nil {
		return err
	}
	tp, shutdown := telemetry.Setup(proc, otlpProc)
	// Register the W3C trace-context propagator so nested/distributed runs can
	// correlate later. Kept at the CLI boundary rather than in telemetry.Setup
	// so that helper stays free of process-global side effects.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	locs := runLocations{workflow: opts.file, logPath: logPath, outputPath: outputPath, cacheDir: cacheDir}
	if otlpProc != nil {
		locs.otlpLabel = otlpLabel
	}

	// Print where everything lives up front so the run can be monitored while
	// it is still in flight (tail the log, watch the cache, etc.).
	if !opts.quiet {
		fmt.Fprintf(stderr, "tp run %s\n", runID)
		locs.writePaths(stderr, true)
	}

	var stopTail context.CancelFunc
	var tailDone <-chan struct{}
	if !opts.quiet {
		tailCtx, cancel := context.WithCancel(context.Background())
		stopTail = cancel
		tailDone = startLogTail(tailCtx, logPath, state, stderr)
	}

	rt := builtin.RuntimeRegistry()
	providers := provider.Default(opts.limits)
	// Providers may own process-level resources (the Copilot provider shares a
	// single CLI server across invocations); release them once the run ends.
	defer providers.Close()
	rt.SetProviders(providers)
	runner := engine.New(res.Doc, rt, store, tp)

	// Translate Ctrl-C / SIGTERM into context cancellation so an interrupted run
	// unwinds through the normal return path. That lets the deferred
	// providers.Close (and each task's deferred session cleanup) release the
	// shared CLI server and its sessions instead of orphaning the process.
	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	started := time.Now()
	out, err := runner.Run(runCtx, engine.Options{
		RunID:       runID,
		Input:       input,
		DryRun:      opts.dryRun,
		MaxParallel: opts.maxParallel,
	})
	elapsed := time.Since(started)
	shutdownErr := shutdown(context.Background())
	if stopTail != nil {
		time.Sleep(100 * time.Millisecond)
		stopTail()
		<-tailDone
	}

	// Persist the final output next to the log so it can be found after the run,
	// then encode it to stdout for piping. Skipped when the run failed.
	var outputErr error
	var encoded []byte
	if err == nil {
		encoded, outputErr = json.MarshalIndent(out, "", "  ")
		if outputErr == nil {
			outputErr = os.WriteFile(outputPath, append(encoded, '\n'), 0o644)
		}
	}

	if !opts.quiet {
		stats := runner.Stats()
		if err != nil {
			fmt.Fprintf(stderr, "tp run %s FAILED in %s\n", runID, elapsed.Round(time.Millisecond))
			// Surface the root cause prominently. The error returned by Run is the
			// message propagated up through every parent span; the trace log pins it
			// to the node, task, and stage where it actually originated -- the same
			// source of truth (and earliest-end-error ordering) that `tp log`
			// and the debug skill use.
			if rec, ok := rootCauseFromLog(logPath); ok {
				fmt.Fprintf(stderr, "  root cause: node %q (%s) failed at stage %s\n",
					attrStr(rec, telemetry.AttrNodeName), attrStr(rec, telemetry.AttrTask), attrStr(rec, telemetry.AttrStage))
				fmt.Fprintf(stderr, "  error:      %s\n", rec.StatusMsg)
			} else {
				fmt.Fprintf(stderr, "  error:      %s\n", err)
			}
			fmt.Fprintf(stderr, "  cache:    %d hit(s), %d miss(es)\n", stats.CacheHits, stats.CacheMisses)
			locs.writePaths(stderr, false)
			fmt.Fprintf(stderr, "  debug:    tp log %s\n", runID)
		} else {
			fmt.Fprintf(stderr, "tp run %s succeeded in %s\n", runID, elapsed.Round(time.Millisecond))
			fmt.Fprintf(stderr, "  cache:    %d hit(s), %d miss(es)\n", stats.CacheHits, stats.CacheMisses)
			locs.writePaths(stderr, true)
		}
	}

	if err != nil {
		return err
	}
	if shutdownErr != nil {
		return shutdownErr
	}
	if outputErr != nil {
		return outputErr
	}
	_, err = stdout.Write(append(encoded, '\n'))
	return err
}

// otlpProcessor builds a batch span processor that exports the run's spans.
// Export is opt-in and enabled when either --otlp-endpoint is given OR the
// standard OpenTelemetry OTEL_TRACES_EXPORTER / OTEL_EXPORTER_OTLP_ENDPOINT
// environment variables are set. When disabled it returns a nil processor,
// which telemetry.Setup skips. The returned label describes the active target
// for the run header.
//
// Precedence: an explicit --otlp-endpoint flag wins and forces OTLP/HTTP to that
// endpoint (a bare host:port is insecure http; a full http:// / https:// URL
// selects TLS from the scheme). With no flag, exporter and protocol selection
// are delegated to the OTel environment via autoexport, so
// OTEL_TRACES_EXPORTER=otlp|console|none and OTEL_EXPORTER_OTLP_PROTOCOL=
// grpc|http/protobuf all take effect.
func otlpProcessor(ctx context.Context, endpoint string) (sdktrace.SpanProcessor, string, error) {
	if endpoint != "" {
		opts := []otlptracehttp.Option{}
		insecure := true
		host := endpoint
		if strings.Contains(endpoint, "://") {
			u, err := url.Parse(endpoint)
			if err != nil {
				return nil, "", fmt.Errorf("invalid --otlp-endpoint %q: %w", endpoint, err)
			}
			host = u.Host
			insecure = u.Scheme != "https"
			if u.Path != "" && u.Path != "/" {
				opts = append(opts, otlptracehttp.WithURLPath(u.Path))
			}
		}
		opts = append(opts, otlptracehttp.WithEndpoint(host))
		if insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exp, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, "", fmt.Errorf("create OTLP exporter: %w", err)
		}
		return sdktrace.NewBatchSpanProcessor(exp), endpoint, nil
	}

	// No flag: enable only if the user opted in via standard OTel env vars.
	// autoexport would otherwise default to OTLP on localhost, which would make
	// every run try to export; gate on the env vars to keep export opt-in.
	if os.Getenv("OTEL_TRACES_EXPORTER") == "" && os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return nil, "", nil
	}
	exp, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("create span exporter from environment: %w", err)
	}
	label := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if label == "" {
		label = "env-configured (OTEL_TRACES_EXPORTER=" + os.Getenv("OTEL_TRACES_EXPORTER") + ")"
	}
	return sdktrace.NewBatchSpanProcessor(exp), label, nil
}

func rootInput(res *verify.VerifiedDocument, opts runOptions) (map[string]any, error) {
	input := map[string]any{}
	if opts.inputFile != "" {
		raw, err := readInput(opts.inputFile)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, fmt.Errorf("parse --input: %w", err)
		}
	}
	entry := res.Doc.Tasks[res.Doc.Entry]
	for k, v := range opts.dynamic {
		coerced, err := coerceFlag(entry.InputSchema, k, v)
		if err != nil {
			return nil, err
		}
		input[k] = coerced
	}
	for k, v := range opts.jsonValues {
		input[k] = v
	}
	input = schema.ApplyDefaults(entry.InputSchema, input)
	if err := schema.Validate(entry.InputSchema, input); err != nil {
		return nil, fmt.Errorf("root input schema violation: %w", err)
	}
	return input, nil
}

func coerceFlag(rootSchema any, key string, raw string) (any, error) {
	var t string
	if s, ok := rootSchema.(map[string]any); ok {
		if props, ok := s["properties"].(map[string]any); ok {
			if propSchema, ok := props[key].(map[string]any); ok {
				t, _ = propSchema["type"].(string)
			}
		}
	}
	switch t {
	case "integer":
		return strconv.Atoi(raw)
	case "number":
		return strconv.ParseFloat(raw, 64)
	case "boolean":
		return strconv.ParseBool(raw)
	case "array", "object":
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("--%s expects JSON %s: %w", key, t, err)
		}
		return v, nil
	default:
		if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
			var v any
			if err := json.Unmarshal([]byte(raw), &v); err == nil {
				return v, nil
			}
		}
		return raw, nil
	}
}

func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// verifyOptions holds the arguments for `tp verify`, which only needs the root
// task file. Keeping this parser separate from parseRunArgs means run-only
// flags never leak into verify and verify-only flags stay local to this parser.
type verifyOptions struct {
	file string
}

func parseVerifyArgs(args []string) (verifyOptions, error) {
	var opts verifyOptions
	if len(args) == 0 {
		return opts, errors.New("missing root task file")
	}
	opts.file = args[0]
	if len(args) > 1 {
		return opts, fmt.Errorf("unexpected argument %q", args[1])
	}
	return opts, nil
}

func verifyWorkflow(args []string, stdout io.Writer) error {
	opts, err := parseVerifyArgs(args)
	if err != nil {
		return err
	}
	res, err := loadVerified(opts.file)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "verified %s: entry=%s tasks=%d\n", opts.file, res.Doc.Entry, len(res.Doc.Tasks))
	return nil
}

// graphOptions holds the arguments for `tp graph`: the root task file and the
// output format. Kept separate from parseRunArgs so graph-only flags such as
// --format live only alongside the graph command.
type graphOptions struct {
	file   string
	format string
}

func parseGraphArgs(args []string) (graphOptions, error) {
	opts := graphOptions{format: "dot"}
	if len(args) == 0 {
		return opts, errors.New("missing root task file")
	}
	opts.file = args[0]
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--format":
			i++
			if i >= len(args) {
				return opts, errors.New("--format requires a value")
			}
			opts.format = args[i]
		default:
			return opts, fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	return opts, nil
}

func graphWorkflow(args []string, stdout io.Writer) error {
	opts, err := parseGraphArgs(args)
	if err != nil {
		return err
	}
	res, err := loadVerified(opts.file)
	if err != nil {
		return err
	}
	if opts.format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res.Graphs[res.Doc.Entry])
	}
	if opts.format != "dot" {
		return fmt.Errorf("unsupported graph format %q", opts.format)
	}
	fmt.Fprintln(stdout, "digraph taskpilot {")
	for _, e := range res.Graphs[res.Doc.Entry].Edges {
		fmt.Fprintf(stdout, "  %q -> %q;\n", e.From, e.To)
	}
	for _, n := range res.Graphs[res.Doc.Entry].Order {
		fmt.Fprintf(stdout, "  %q;\n", n)
	}
	fmt.Fprintln(stdout, "}")
	return nil
}

func builtinCommand(args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] != "list" {
		return errors.New("usage: tp builtin list")
	}
	for _, spec := range builtin.SchemaRegistry().All() {
		fmt.Fprintf(stdout, "%s\t%s\n", spec.Name, spec.Version)
	}
	return nil
}

func cacheCommand(args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] != "gc" {
		return errors.New("usage: tp cache gc")
	}
	state, err := cache.ResolveStateDir()
	if err != nil {
		return err
	}
	store := cache.New(cache.CacheDir(state))
	removed, err := store.GC(cache.DefaultClaimMaxAge)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "removed %d stale cache item(s)\n", removed)
	return nil
}

// pathsCommand prints the locations tp resolves at runtime so external
// tooling (e.g. the debug-taskpilot-run skill) can consume them instead of
// re-deriving the state-dir fallback rules. Output is "<name>\t<dir>" lines,
// keeping cache.ResolveStateDir the single source of truth.
func pathsCommand(args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: tp paths")
	}
	state, err := cache.ResolveStateDir()
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "state\t%s\n", state)
	fmt.Fprintf(stdout, "logs\t%s\n", filepath.Join(state, "logs"))
	fmt.Fprintf(stdout, "cache\t%s\n", cache.CacheDir(state))
	return nil
}

type logOptions struct {
	runID string
	json  bool
	tail  bool
}

func parseLogArgs(args []string) (logOptions, error) {
	var opts logOptions
	if len(args) == 0 {
		return opts, errors.New("usage: tp log <run-id> [--json] [--tail]")
	}
	opts.runID = args[0]
	if !validRunID(opts.runID) {
		return opts, fmt.Errorf("invalid run id %q", opts.runID)
	}
	for _, arg := range args[1:] {
		switch arg {
		case "--json":
			opts.json = true
		case "--tail":
			opts.tail = true
		default:
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}
	}
	return opts, nil
}

func logCommand(args []string, stdout io.Writer) error {
	opts, err := parseLogArgs(args)
	if err != nil {
		return err
	}
	state, err := cache.ResolveStateDir()
	if err != nil {
		return err
	}
	logPath := tflog.LogPath(filepath.Join(state, "logs"), opts.runID)
	if _, err := os.Stat(logPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("log for run %q not found", opts.runID)
		}
		return err
	}
	if opts.tail {
		return streamLog(context.Background(), logPath, state, stdout, opts.json, true)
	}
	return streamLog(context.Background(), logPath, state, stdout, opts.json, false)
}

func startLogTail(ctx context.Context, logPath, state string, w io.Writer) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = streamLog(ctx, logPath, state, w, false, true)
	}()
	return done
}

func streamLog(ctx context.Context, logPath, state string, w io.Writer, jsonMode, follow bool) error {
	r := newLogRenderer(state, !jsonMode && colorEnabled(w))
	var offset int64
	for {
		nextOffset, err := readLogFrom(logPath, offset, r, w, jsonMode)
		if err != nil {
			return err
		}
		offset = nextOffset
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			_, err := readLogFrom(logPath, offset, r, w, jsonMode)
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func readLogFrom(logPath string, offset int64, r *logRenderer, w io.Writer, jsonMode bool) (int64, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return offset, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	scanner := tflog.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		offset += int64(len(line)) + 1
		if jsonMode {
			fmt.Fprintln(w, line)
			continue
		}
		if err := r.print(w, line); err != nil {
			fmt.Fprintf(w, "unparseable log event: %v\n", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return offset, err
	}
	return offset, nil
}

// colorEnabled reports whether ANSI styling should be used for w. Styling is
// applied only when the destination is an interactive terminal (a character
// device) and the user has not opted out via the conventional NO_COLOR
// variable. Buffers, pipes, and files therefore receive the plain, script
// friendly format unchanged.
func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// palette holds ANSI SGR codes, all empty when color is disabled so wrap is a
// no-op.
type palette struct {
	reset, bold, dim, red, green, cyan string
}

func newPalette() palette {
	return palette{
		reset: "\x1b[0m",
		bold:  "\x1b[1m",
		dim:   "\x1b[2m",
		red:   "\x1b[31m",
		green: "\x1b[32m",
		cyan:  "\x1b[36m",
	}
}

func (p palette) wrap(code, s string) string {
	if code == "" || s == "" {
		return s
	}
	return code + s + p.reset
}

// glyphs are the status markers used in the pretty (terminal) renderer.
const (
	glyphRun   = "\u25cf" // filled circle: run lifecycle
	glyphStart = "\u25b6" // right triangle: node/loop started
	glyphOK    = "\u2713" // check: completed
	glyphFail  = "\u2717" // ballot x: failed
	glyphLoop  = "\u21bb" // clockwise arrow: forEach/loop fan-out
	glyphEvent = "\u00b7" // middle dot: span event / continuation
)

// stampWidth is the fixed column width of the relative timestamp ("+  1.234s").
const stampWidth = 9

// logRenderer turns streamed SpanRecords into human output. When color is off
// it preserves the historical plain, line-oriented format. When color is on it
// emits an indented, colorized tree keyed off span parentage, with timestamps
// shown relative to the first record (the run start) so an in-progress run is
// easy to read at a glance.
type logRenderer struct {
	state string
	color bool
	pal   palette
	depth map[string]int // spanID -> indent depth
	start time.Time      // first observed timestamp; baseline for relative stamps
}

func newLogRenderer(state string, color bool) *logRenderer {
	r := &logRenderer{state: state, color: color, depth: map[string]int{}}
	if color {
		r.pal = newPalette()
	}
	return r
}

func (r *logRenderer) print(w io.Writer, line string) error {
	rec, err := tflog.DecodeSpanRecord([]byte(line))
	if err != nil {
		return err
	}
	if !r.color {
		return printLogEvent(w, r.state, rec)
	}
	return r.printPretty(w, rec)
}

// track returns the indent depth for rec and maintains the spanID->depth map.
// A start record nests one level under its parent (depth 0 when the parent is
// unknown, e.g. the root run); the matching end record reuses and releases that
// depth.
func (r *logRenderer) track(rec telemetry.SpanRecord) int {
	if rec.Phase == telemetry.PhaseStart {
		d := 0
		if rec.ParentID != "" {
			if pd, ok := r.depth[rec.ParentID]; ok {
				d = pd + 1
			}
		}
		if rec.SpanID != "" {
			r.depth[rec.SpanID] = d
		}
		return d
	}
	if rec.SpanID != "" {
		if d, ok := r.depth[rec.SpanID]; ok {
			delete(r.depth, rec.SpanID)
			return d
		}
	}
	return 0
}

// stamp formats t as a dim, minimum-width offset from the run start.
func (r *logRenderer) stamp(t time.Time) string {
	if r.start.IsZero() && !t.IsZero() {
		r.start = t
	}
	d := t.Sub(r.start)
	if d < 0 {
		d = 0
	}
	return r.pal.wrap(r.pal.dim, fmt.Sprintf("+%6.3fs", d.Seconds()))
}

func (r *logRenderer) blankStamp() string {
	return strings.Repeat(" ", stampWidth)
}

func (r *logRenderer) printPretty(w io.Writer, rec telemetry.SpanRecord) error {
	depth := r.track(rec)
	indent := strings.Repeat("  ", depth)
	pc := prettyCtx{
		r: r,
		p: r.pal,
		emit: func(t time.Time, content string) {
			fmt.Fprintf(w, "%s  %s%s\n", r.stamp(t), indent, content)
		},
		cont: func(content string) {
			fmt.Fprintf(w, "%s  %s%s\n", r.blankStamp(), indent, content)
		},
	}
	return rendererFor(rec).pretty(pc, rec)
}

// prettyCtx carries the shared state a spanRenderer needs to emit the pretty
// (colorized, indented) form of a record: the palette, the indent-aware emit
// and continuation writers, and the owning renderer for depth/state lookups.
type prettyCtx struct {
	r    *logRenderer
	p    palette
	emit func(t time.Time, content string)
	cont func(content string)
}

// spanRenderer renders one span kind in both the plain, line-oriented form and
// the pretty, colorized form. rendererFor is the single semantic dispatch point
// mapping a record's span kind to its renderer, so wiring up a new span kind
// touches exactly one switch instead of one per output format.
type spanRenderer interface {
	plain(w io.Writer, state string, rec telemetry.SpanRecord) error
	pretty(pc prettyCtx, rec telemetry.SpanRecord) error
}

func rendererFor(rec telemetry.SpanRecord) spanRenderer {
	switch spanKind(rec) {
	case telemetry.SpanKindRun:
		return runSpanRenderer{}
	case telemetry.SpanKindNode:
		return nodeSpanRenderer{}
	case telemetry.SpanKindForEach, telemetry.SpanKindLoop:
		return loopSpanRenderer{}
	default:
		return eventSpanRenderer{}
	}
}

type runSpanRenderer struct{}

func (runSpanRenderer) plain(w io.Writer, _ string, rec telemetry.SpanRecord) error {
	runID := attrStr(rec, telemetry.AttrRunID)
	if rec.Phase == telemetry.PhaseStart {
		fmt.Fprintf(w, "%s run started entry=%s run=%s\n", tsStart(rec), attrStr(rec, telemetry.AttrEntry), runID)
		return nil
	}
	if rec.Status == telemetry.StatusError {
		fmt.Fprintf(w, "%s run failed run=%s error=%s\n", tsEnd(rec), runID, rec.StatusMsg)
	} else {
		fmt.Fprintf(w, "%s run completed run=%s\n", tsEnd(rec), runID)
	}
	return nil
}

func (runSpanRenderer) pretty(pc prettyCtx, rec telemetry.SpanRecord) error {
	p := pc.p
	runID := attrStr(rec, telemetry.AttrRunID)
	if rec.Phase == telemetry.PhaseStart {
		entry := attrStr(rec, telemetry.AttrEntry)
		pc.emit(rec.StartTime, fmt.Sprintf("%s %s %s",
			p.wrap(p.cyan, glyphRun),
			p.wrap(p.bold, "run "+entry),
			p.wrap(p.dim, runID)))
		return nil
	}
	if rec.Status == telemetry.StatusError {
		pc.emit(spanEnd(rec), fmt.Sprintf("%s %s %s",
			p.wrap(p.red, glyphFail),
			p.wrap(p.bold, "run failed"),
			p.wrap(p.red, rec.StatusMsg)))
		return nil
	}
	pc.emit(spanEnd(rec), fmt.Sprintf("%s %s %s",
		p.wrap(p.green, glyphOK),
		p.wrap(p.bold, "run completed"),
		p.wrap(p.dim, spanDur(rec))))
	return nil
}

type nodeSpanRenderer struct{}

func (nodeSpanRenderer) plain(w io.Writer, state string, rec telemetry.SpanRecord) error {
	node := attrStr(rec, telemetry.AttrNodeName)
	task := attrStr(rec, telemetry.AttrTask)
	nodeID := attrStr(rec, telemetry.AttrNodeID)
	if rec.Phase == telemetry.PhaseStart {
		fmt.Fprintf(w, "%s node started %s task=%s nodeId=%s\n", tsStart(rec), node, task, nodeID)
		return nil
	}
	if rec.Status == telemetry.StatusError {
		fmt.Fprintf(w, "%s node failed %s task=%s nodeId=%s stage=%s durationMs=%d error=%s\n",
			tsEnd(rec), node, task, nodeID, attrStr(rec, telemetry.AttrStage), rec.DurationMs, rec.StatusMsg)
		printSpanEvents(w, rec)
		return nil
	}
	fmt.Fprintf(w, "%s node.completed %s task=%s nodeId=%s cache=%s durationMs=%d\n",
		tsEnd(rec), node, task, nodeID, attrStr(rec, telemetry.AttrCacheStatus), rec.DurationMs)
	printSpanEvents(w, rec)
	if preview, ok := outputPreview(state, rec); ok {
		fmt.Fprintf(w, "  output: %s\n", preview)
	}
	return nil
}

func (nodeSpanRenderer) pretty(pc prettyCtx, rec telemetry.SpanRecord) error {
	p := pc.p
	node := attrStr(rec, telemetry.AttrNodeName)
	task := attrStr(rec, telemetry.AttrTask)
	if rec.Phase == telemetry.PhaseStart {
		pc.emit(rec.StartTime, fmt.Sprintf("%s %s %s",
			p.wrap(p.cyan, glyphStart),
			p.wrap(p.bold, node),
			p.wrap(p.dim, task)))
		return nil
	}
	if rec.Status == telemetry.StatusError {
		stage := attrStr(rec, telemetry.AttrStage)
		detail := rec.StatusMsg
		if stage != "" {
			detail = stage + ": " + detail
		}
		pc.emit(spanEnd(rec), fmt.Sprintf("%s %s %s %s %s",
			p.wrap(p.red, glyphFail),
			p.wrap(p.bold, node),
			p.wrap(p.dim, task),
			p.wrap(p.dim, spanDur(rec)),
			p.wrap(p.red, detail)))
		pc.r.prettyEvents(pc.cont, rec)
		return nil
	}
	pc.emit(spanEnd(rec), fmt.Sprintf("%s %s %s %s %s",
		p.wrap(p.green, glyphOK),
		p.wrap(p.bold, node),
		p.wrap(p.dim, task),
		p.wrap(p.dim, spanDur(rec)),
		p.wrap(p.dim, "("+cacheLabel(attrStr(rec, telemetry.AttrCacheStatus))+")")))
	pc.r.prettyEvents(pc.cont, rec)
	if preview, ok := outputPreview(pc.r.state, rec); ok {
		pc.cont(p.wrap(p.dim, "output: "+preview))
	}
	return nil
}

type loopSpanRenderer struct{}

func (loopSpanRenderer) plain(w io.Writer, _ string, rec telemetry.SpanRecord) error {
	node := attrStr(rec, telemetry.AttrNodeName)
	kind := attrStr(rec, telemetry.AttrSpanKind)
	if rec.Phase == telemetry.PhaseStart {
		count := attrStr(rec, telemetry.AttrForEachCount)
		fmt.Fprintf(w, "%s %s started %s count=%s\n", tsStart(rec), kind, node, count)
		return nil
	}
	if rec.Status == telemetry.StatusError {
		fmt.Fprintf(w, "%s %s failed %s durationMs=%d error=%s\n", tsEnd(rec), kind, node, rec.DurationMs, rec.StatusMsg)
		return nil
	}
	fmt.Fprintf(w, "%s %s completed %s durationMs=%d\n", tsEnd(rec), kind, node, rec.DurationMs)
	return nil
}

func (loopSpanRenderer) pretty(pc prettyCtx, rec telemetry.SpanRecord) error {
	p := pc.p
	node := attrStr(rec, telemetry.AttrNodeName)
	if rec.Phase == telemetry.PhaseStart {
		label := node
		if count := attrStr(rec, telemetry.AttrForEachCount); count != "" {
			label += " x" + count
		}
		pc.emit(rec.StartTime, fmt.Sprintf("%s %s",
			p.wrap(p.cyan, glyphLoop),
			p.wrap(p.bold, label)))
		return nil
	}
	if rec.Status == telemetry.StatusError {
		pc.emit(spanEnd(rec), fmt.Sprintf("%s %s %s %s",
			p.wrap(p.red, glyphFail),
			p.wrap(p.bold, node),
			p.wrap(p.dim, spanDur(rec)),
			p.wrap(p.red, rec.StatusMsg)))
		return nil
	}
	pc.emit(spanEnd(rec), fmt.Sprintf("%s %s %s",
		p.wrap(p.green, glyphOK),
		p.wrap(p.bold, node),
		p.wrap(p.dim, spanDur(rec))))
	return nil
}

type eventSpanRenderer struct{}

func (eventSpanRenderer) plain(w io.Writer, _ string, rec telemetry.SpanRecord) error {
	fmt.Fprintf(w, "%s %s %s\n", tsStart(rec), rec.Phase, rec.Name)
	return nil
}

func (eventSpanRenderer) pretty(pc prettyCtx, rec telemetry.SpanRecord) error {
	p := pc.p
	pc.emit(rec.StartTime, p.wrap(p.dim, glyphEvent+" "+rec.Phase+" "+rec.Name))
	return nil
}

func (r *logRenderer) prettyEvents(cont func(string), rec telemetry.SpanRecord) {
	p := r.pal
	for _, ev := range rec.Events {
		cont(p.wrap(p.dim, glyphEvent+" "+ev.Name))
	}
}

func spanEnd(rec telemetry.SpanRecord) time.Time {
	if rec.EndTime != nil {
		return *rec.EndTime
	}
	return rec.StartTime
}

func spanDur(rec telemetry.SpanRecord) string {
	return fmt.Sprintf("%dms", rec.DurationMs)
}

// cacheLabel renders a cache-status attribute as a short human phrase.
func cacheLabel(status string) string {
	switch status {
	case telemetry.CacheStatusHit:
		return "cache hit"
	case telemetry.CacheStatusMiss:
		return "cache miss"
	case telemetry.CacheStatusDryRun:
		return "dry run"
	case telemetry.CacheStatusNonCacheable:
		return "no cache"
	case "":
		return "ran"
	default:
		return status
	}
}

// rootCauseFromLog scans a completed run's JSONL trace and returns the node
// span that is the true root cause of a failure: the earliest-ending node-kind
// span with an error status. A node error propagates up through every parent
// (its subgraph, any forEach, and the top-level run) and fans out across
// sibling branches, so the authoritative root cause is the first node to
// actually fail, ordered by the span end timestamps the log records -- the same
// rule the debug skill applies. ok is false when no failing node span is
// present (e.g. the run failed before any node ran), in which case the caller
// should fall back to the propagated error.
func rootCauseFromLog(logPath string) (telemetry.SpanRecord, bool) {
	f, err := os.Open(logPath)
	if err != nil {
		return telemetry.SpanRecord{}, false
	}
	defer f.Close()
	records, err := tflog.ReadSpanRecords(f)
	if err != nil {
		return telemetry.SpanRecord{}, false
	}
	var best telemetry.SpanRecord
	found := false
	for _, rec := range records {
		if rec.Phase != telemetry.PhaseEnd || rec.Status != telemetry.StatusError || spanKind(rec) != telemetry.SpanKindNode {
			continue
		}
		if !found || spanEnd(rec).Before(spanEnd(best)) {
			best, found = rec, true
		}
	}
	return best, found
}

func printLogEvent(w io.Writer, state string, rec telemetry.SpanRecord) error {
	return rendererFor(rec).plain(w, state, rec)
}

// spanKind returns the stable span-kind discriminator. It prefers the explicit
// AttrSpanKind attribute and falls back to the legacy span name for records
// written before the attribute existed.
func spanKind(rec telemetry.SpanRecord) string {
	if k := attrStr(rec, telemetry.AttrSpanKind); k != "" {
		return k
	}
	switch rec.Name {
	case telemetry.SpanRun:
		return telemetry.SpanKindRun
	case telemetry.SpanNode:
		return telemetry.SpanKindNode
	}
	return ""
}

func printSpanEvents(w io.Writer, rec telemetry.SpanRecord) {
	for _, ev := range rec.Events {
		fmt.Fprintf(w, "  event: %s\n", ev.Name)
	}
}

func outputPreview(state string, rec telemetry.SpanRecord) (string, bool) {
	rel := attrStr(rec, telemetry.AttrCachePath)
	if rel == "" {
		return "", false
	}
	full, err := resolveStateRelative(state, rel)
	if err != nil {
		return "", false
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return "", false
	}
	var entry cache.Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return "", false
	}
	preview, err := json.Marshal(entry.Output)
	if err != nil {
		return "", false
	}
	const maxPreview = 512
	if len(preview) > maxPreview {
		return string(preview[:maxPreview]) + "...", true
	}
	return string(preview), true
}

// logTimeLayout is a full UTC timestamp with date and milliseconds. Runs can
// span days or weeks, so a time-only stamp would be ambiguous; the trailing
// Z07:00 makes the UTC offset explicit for correlation with other systems.
const logTimeLayout = "2006-01-02T15:04:05.000Z07:00"

func tsStart(rec telemetry.SpanRecord) string {
	return rec.StartTime.UTC().Format(logTimeLayout)
}

func tsEnd(rec telemetry.SpanRecord) string {
	return spanEnd(rec).UTC().Format(logTimeLayout)
}

func attrStr(rec telemetry.SpanRecord, key string) string {
	if rec.Attributes == nil {
		return ""
	}
	if v, ok := rec.Attributes[key]; ok {
		return v.String()
	}
	return ""
}

func validRunID(runID string) bool {
	if runID == "" {
		return false
	}
	for _, r := range runID {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return filepath.Base(runID) == runID
}

func resolveStateRelative(state, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", errors.New("path must be relative to state dir")
	}
	full := filepath.Clean(filepath.Join(state, rel))
	root := filepath.Clean(state)
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", errors.New("path escapes state dir")
	}
	return full, nil
}
