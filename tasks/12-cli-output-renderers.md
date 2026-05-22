Parent slice: [cli](../slices/cli.md)
Depends on: 11

# Task 12 — cli-output-renderers

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Three rendering paths (table, JSON, YAML) with NDJSON for streaming RPCs, plus a typed `Error` renderer used by every non-`serve` subcommand.

## Tasks
- [ ] Create `internal/cliclient/output.go` with `RenderTable(w io.Writer, headers []string, rows [][]string)`, `RenderJSON(w io.Writer, v any)`, `RenderYAML(w io.Writer, v any)`.
- [ ] Table rules: no wrapping; cells truncated to terminal width with `…`; ANSI color only when `isatty.IsTerminal(stdout)`; right-pad with spaces.
- [ ] JSON: `json.MarshalIndent` with 2-space indent; key order alphabetical (custom marshaler if necessary).
- [ ] YAML: `gopkg.in/yaml.v3` with indent 2; same data shape as JSON.
- [ ] Streaming: add `RenderJSONStream(w io.Writer, ch <-chan any)` writing one compact JSON object per line (NDJSON), flushing after each line.
- [ ] Create `internal/cliclient/errors.go` with `RenderError(w io.Writer, e *pb.Error)`: writes `Error: <code> — <message>` to stderr followed by sorted `<k>=<v>` lines from `details`; exits with code 1 via `os.Exit`. Transport-layer failures render as `Connection error: <addr>: <reason>`.
- [ ] Unit tests with golden-file fixtures under `internal/cliclient/testdata/renderers/`.

## Acceptance criteria
- [ ] `go test ./internal/cliclient/... -race -count=1 -run Render` exits 0.
- [ ] Test asserts `RenderJSON` output is alphabetically key-sorted via `jq '.|keys'`.
- [ ] `git grep -nE 'NDJSON|RenderJSONStream' internal/cliclient/` matches.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
