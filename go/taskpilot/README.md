# TaskPilot

TaskPilot is a lightweight workflow runner for schema-validated task graphs. It is intended to help developer's automate repeatable workflows, including workflows that mix deterministic steps -- like building and running tests -- with controlled LLM reasoning.

## How it works

A TaskPilot workflow is expressed as a graph of tasks, where each task executes a step like running a shell script, invoking a Copilot agent, or running a nested workflow.

TaskPilot builds and verifies the graph before execution, runs independent nodes in parallel, and caches task outputs by content-addressed inputs.

## Quick start

Build the binary (outputs `tp.exe` in the current directory), or install it onto your PATH:

```powershell
go build -o tp.exe ./cmd/tp   # build into repo root
go install ./cmd/tp           # install into $(go env GOPATH)\bin
```

```powershell
tp verify examples\hello.yaml
tp run examples\hello.yaml --name world
tp run examples\hello.yaml --name world --quiet
tp log <run-id>
tp run examples\pwsh-dry-run.yaml --root D:\taskpoint --depth 2 --dry-run
tp graph examples\hello.yaml --format dot
tp run examples\comment-review.yaml --root D:\taskpoint --pattern "internal/provider/*.go" --copilot-parallel 3
tp builtin list
```

Mutable runtime state defaults to `%LOCALAPPDATA%\taskpilot` and can be overridden with `TASKPILOT_STATE_DIR`.

`tp log <run-id>` pretty-prints a persisted run, `--tail` follows it, and `--json` emits the raw span records.

## Parallelism and throttling

Fan-out in the task graph — multiple ready nodes, `forEach`, and JSONL maps — is implicit parallelism. Two layers bound how much runs at once:

- **Engine ceiling (`--max-parallel`, default 64):** a coarse, graph-wide safety limit on how many nodes execute concurrently.
- **Per-provider concurrency caps:** side-effecting work runs through providers, and each provider is the real throttle.

Provider caps are configured per run:

- `--copilot-parallel N` — max concurrent Copilot invocations (scales to available memory).
- `--pwsh-parallel N` — max concurrent PowerShell executions (default 8).
