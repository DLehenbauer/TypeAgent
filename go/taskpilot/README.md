# TaskPilot

TaskPilot is a lightweight workflow runner for schema-validated task graphs. It is intended to help developer's automate repeatable workflows, including workflows that mix deterministic steps -- like building and running tests -- with controlled LLM reasoning.

## How it works

A TaskPilot workflow is expressed as a graph of tasks, where each task executes a step like running a shell script, invoking a Copilot agent, or running a nested workflow.

TaskPilot builds and verifies the graph before execution, runs independent nodes in parallel, and caches task outputs by content-addressed inputs.

## Quick start

### 1. Check out the source

TaskPilot lives under the `go/` subtree of the [DLehenbauer/TypeAgent](https://github.com/DLehenbauer/TypeAgent/tree/taskpilot) fork.

Clone the `taskpilot` branch and change into the TaskPilot directory:

```powershell
git clone --branch taskpilot https://github.com/DLehenbauer/TypeAgent.git
cd TypeAgent/go/taskpilot
```

Alternatively, use a sparse, shallow checkout to fetch just the `go/` subtree:

```powershell
git clone --filter=blob:none --no-checkout --depth 1 --branch taskpilot https://github.com/DLehenbauer/TypeAgent.git
cd TypeAgent
git sparse-checkout init --cone
git sparse-checkout set go
git checkout taskpilot
cd go/taskpilot
```

### 2. Install the Go toolchain

TaskPilot requires Go 1.22 or later.

```powershell
# Windows
winget install --id GoLang.Go -e
```

```bash
# macOS (Homebrew)
brew install go

# Linux (Debian/Ubuntu)
sudo apt-get update && sudo apt-get install -y golang-go
```

Verify the toolchain is on your PATH:

```powershell
go version
```

### 3. Install TaskPilot

Install the `tp` binary onto your PATH (`go install` compiles and installs in one step):

```powershell
go install ./cmd/tp           # install into $(go env GOPATH)\bin
```

Make sure `$(go env GOPATH)\bin` (Windows) or `$(go env GOPATH)/bin` (macOS/Linux) is on your PATH so the installed `tp` binary is available.

### 4. Verify the installation

Confirm `tp` is on your PATH and prints its version:

```powershell
tp version
```

### 5. Run a workflow

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
- `--hyperv-parallel N` — max concurrent Hyper-V target operations on Windows (default 1).

## Cache control

A host `pwsh.run` is memoized by default. Set `cache: false` when its purpose is a side effect that must be performed on every run:

```yaml
- task: pwsh.run
  inputs:
    script: ./scripts/New-HostShare.ps1
    cache: false
```

Filesystem-observing builtins such as `file.glob` remain memoized: a digest of the observed state participates in node identity. Lease lifecycle and lease-bound runs always execute because their process-local effects cannot be replayed from the engine cache.

## Execution targets and leases

Host execution is the absence of a lease. Bind one with `runsOn` and the same body runs on a durable target instead:

```yaml
- task: lease.acquire
  inputs:
    kind: hyperv
    options:
      baseImage: C:\images\windows.vhdx
      guest:
        username: Administrator
        passwordEnv: HYPERV_GUEST_PASSWORD
        winRMPort: 5985
        readinessTimeoutSeconds: 300
        connectionTimeoutSeconds: 5                      # -> a $lease

- task: pwsh.run
  inputs:
    runsOn: { $from: node, node: vm, path: [lease] }
    script: ./scripts/Apply-Config.ps1
    checkpoint: true                                      # advances the lease's state

- task: lease.release
  inputs: { lease: { $from: node, node: tests, path: [lease] } }
```

A lease is **linear**: `tp verify` rejects a graph in which a lease has zero or two-plus consumers, or whose chain does not end in `lease.release`. That is what makes exclusive access and total ordering of mutations fall out of dataflow rather than runtime locking — and it turns a forgotten teardown into a verification error rather than a leaked machine. Pass `keep: true` to `lease.release` to leave the context up for inspection.

Every lease-bound run bypasses engine memoization. A run with `checkpoint: true` commits its content-addressed node ID as the successor lease state; a plain `runsOn` run is transient and threads the current state through unchanged.

Adding a target kind (SSH, container, cloud VM) implements the small target backend contract and registers it at the CLI composition root. `tp cache gc` uses the same composition, so every runnable target is also swept.

Hyper-V is a compile-time extension under `extensions/windows/hyperv`; its
PowerShell driver and host/guest helpers are embedded and materialized as one
private bundle per action. Guest and optional workspace passwords are resolved
from caller-named environment variables and are never workflow options.
Operational `guest` and `workspace` settings do not affect checkpoint identity.
Runtime plugins are intentionally deferred. Go's standard plugin mechanism does
not support Windows; a future runtime extension should use a versioned
subprocess protocol.

## Data-plane references

Two explicit reference envelopes flow through task inputs and outputs:

| Envelope | Carries | Identity |
|---|---|---|
| `$file` | path + content fingerprint | whole envelope |
| `$lease` | kind, physical id, logical state | `{kind, state}` — the physical allocation is excluded |

File references render to their paths when passed to PowerShell, templates, or Copilot. Lease references may only be bound to declared lease inputs.
