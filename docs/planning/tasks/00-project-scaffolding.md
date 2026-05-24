Parent slice: [cli](../slices/cli.md)
Depends on: none

# Task 00 — project-scaffolding

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Stand up the Go module, the source tree layout from the slices, and the cobra root command with persistent flags.

## Tasks
- [x] Create `go.mod` declaring `module github.com/PatrickRuddiman/jaco` and `go 1.22`.
- [x] Create `cmd/jaco/main.go` with `func main()` that calls `cmd.Execute()` from the root command.
- [x] Create `cmd/jaco/root.go` defining `rootCmd` (`Use: "jaco"`) and registering persistent flags from cli §4: `--context`, `--output/-o` (default `table`), `--server`, `--quiet/-q`, `--verbose/-v`.
- [x] Create empty `doc.go` files under `internal/controlplane/`, `internal/cliclient/`, `internal/scheduler/`, `internal/runtime/`, `internal/ingress/`, `internal/discovery/`, `internal/packaging/` so each is a valid Go package.
- [x] Create `Makefile` with targets `build`, `test`, `vet`, `lint`, `proto`, `clean`.
- [x] Create `.gitignore` covering `/jaco`, `/dist/`, `*.test`, `*.out`, `coverage.*`.

## Acceptance criteria
- [x] `test -f go.mod && grep -q 'module github.com/PatrickRuddiman/jaco' go.mod`.
- [x] `go build ./...` exits 0.
- [x] `go vet ./...` exits 0.
- [x] `go test ./... -count=1` exits 0.
- [x] `go build -o jaco ./cmd/jaco && ./jaco --help 2>&1 | grep -qE -- '--context'`.
- [x] `for d in internal/controlplane internal/cliclient internal/scheduler internal/runtime internal/ingress internal/discovery internal/packaging; do test -f "$d/doc.go" || exit 1; done` exits 0.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
