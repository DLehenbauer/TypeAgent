---
name: debug-taskpilot-run
description: 'Find and debug a failed taskpilot run from its JSONL trace log. Use when a `tp run`/`verify` fails, when investigating why a workflow errored, diagnosing a failed pwsh.run or copilot.invoke node, or inspecting the latest run log under the taskpilot logs directory. Triggers: "debug the run", "why did the run fail", "check the latest taskpilot log", "what failed".'
argument-hint: 'optional run id (defaults to the latest run)'
---

# Debug a Failed Taskpilot Run

## When to Use

- A `tp run` exited non-zero and you need the root cause.
- You want to inspect the most recent run without knowing its run id.
- A specific node (`pwsh.run`, `copilot.invoke`, `file.glob`, ...) failed and you
  need its error message, stage, and timing.

## Background: where logs live and what they contain

taskpilot writes one JSONL trace file per run:

```
<stateDir>/logs/<runID>.jsonl
```

`<stateDir>` is resolved (see `cache.ResolveStateDir`) as:

1. `$TASKPILOT_STATE_DIR` if set, else
2. `%LOCALAPPDATA%\taskpilot` on Windows, else
3. `~/.taskpilot`.

So on Windows the default is `%LOCALAPPDATA%\taskpilot\logs`. A successful run may
also write `<runID>.output.json` next to the log; a failed run usually has only
the `.jsonl`.

Each line is a `telemetry.SpanRecord`. The fields that matter for debugging:

| field | meaning |
|-------|---------|
| `phase` | `"start"` or `"end"` -- one of each per span |
| `name` | span label, e.g. `preflight (pwsh.run)` or the entry task |
| `status` | `"ok"`, `"error"`, or unset |
| `statusMessage` | the error string when `status == "error"` |
| `durationMs` | wall time for the span (on `end`) |
| `events[]` | exception events; `attributes."exception.message"` holds the message |
| `attributes."taskpilot.span.kind"` | `"run"` (top-level) or `"node"` |
| `attributes."taskpilot.task"` | task type: `pwsh.run`, `copilot.invoke`, ... |
| `attributes."taskpilot.node.name"` | the node id in the workflow graph |
| `attributes."taskpilot.stage"` | where it failed: `input_validation`, `execution`, `output_validation` |
| `attributes."taskpilot.cache.status"` | `hit` or `miss` |

Key insight: when a node fails, the error **propagates upward**. The node span,
its parent subgraph span, and the top-level `run` span all carry the same
`statusMessage`. The **earliest** `end` record with `status == "error"` is the
real root cause; everything after it is propagation.

## Procedure

1. **Find and triage the failing run.** Run the helper, which resolves the logs
   dir, picks the latest run (or one you name), and prints the root-cause span
   first:

   ```pwsh
   ./.github/skills/debug-taskpilot-run/scripts/Find-FailedRun.ps1
   # or a specific run:
   ./.github/skills/debug-taskpilot-run/scripts/Find-FailedRun.ps1 -RunId <run-id>
   # full timeline of every span:
   ./.github/skills/debug-taskpilot-run/scripts/Find-FailedRun.ps1 -All
   ```

   See [Find-FailedRun.ps1](./scripts/Find-FailedRun.ps1).

2. **Or use the built-in renderer** for a human-readable timeline (handy to see
   ordering and `node started` / `node failed` / `run failed` lines):

   ```pwsh
   tp log <run-id>          # pretty
   tp log <run-id> --json   # raw JSONL passthrough
   tp log <run-id> --tail   # follow a live run
   ```

3. **Read the root-cause span.** From the helper's first record, note:
   - `task` -- which provider/builtin failed.
   - `stage` -- `input_validation` (a `$from` ref resolved wrong or schema
     mismatch), `execution` (the command/agent itself failed), or
     `output_validation` (output didn't match `outputSchema`).
   - `error` / `exception` -- the message.

4. **Drill into the task type:**
   - **`pwsh.run`**: the message often comes from the script's stderr or a
     non-zero exit. `pwsh stdout is empty: expected JSON` means the script
     produced no stdout but a JSON result was expected -- inspect the script for
     an early `throw`/`exit`, or run it directly with the same `args` to see the
     real error. Check the node's `Invoke-*.ps1` referenced in the workflow.
   - **`copilot.invoke`**: look for timeouts (`sessionIdleTimeoutSeconds`),
     non-JSON output when `expectJson`/`outputSchema` is set, or permission
     prompts. Re-check the rendered prompt and the `outputSchema`.
   - **`file.ref` / `file.refGlob` / `file.glob` / `path.join`**: usually a bad
     path or empty glob; verify `path`/`root`/`pattern` inputs.

5. **Confirm the workflow wiring.** Open the `.yaml` and locate the failing
   node by its `taskpilot.node.name`. Verify its `inputs`, `$from` references, and
   `outputSchema` against the error stage. For `pwsh.run` nodes, reproduce by
   running the script with the exact `args` shown in the node.

6. **Report** the root cause: run id, failing node + task + stage, the error
   message, the most likely fix, and (when useful) the reproduction command.

## Example Diagnosis

For `run-7dd8511272d6c6f16091e507ecf7ef61`, the earliest error span is the
`preflight` node (`pwsh.run`, stage `execution`) with
`pwsh stdout is empty: expected JSON`; the `selfReview` run span repeats the same
message. Root cause: `Test-Preflight.ps1` returned no stdout (it threw before
emitting its JSON result), not a workflow-graph problem.
