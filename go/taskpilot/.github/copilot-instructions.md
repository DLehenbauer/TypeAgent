# Project Guidelines

## Transient and generated files

Emit generated artifacts (coverage, logs, binaries, scratch output) under
`.tmp/`, never the repo root or package directories. `.tmp/` is git-ignored
(except for `.tmp/.gitkeep`).

- Point output flags at `.tmp/`, e.g. `go test ./... -coverprofile=.tmp/coverage.out`.
- For parallel runs or history worth keeping (ledgers, run-over-run comparison),
  namespace as `.tmp/<task>/<timestamp>/…` with a sortable `YYMMDD-HHMMSS` stamp,
  e.g. `.tmp/coverage/260619-143205/coverage.out`.
- If a tool writes elsewhere and can't be redirected, move or delete its output
  afterward — don't commit it.

## Build and test

Go module (`go 1.24`).

- Build: `go build ./...`
- Test: `go test ./...`
- Generate and inspect coverage with:
  - `go test ./... -coverprofile=.tmp/coverage.out`
  - `go tool cover -func=.tmp/coverage.out` (per-function summary)
  - `go tool cover -html=.tmp/coverage.out` (annotated HTML view)
