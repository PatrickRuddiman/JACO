Parent slice: [cli](../slices/cli.md)
Depends on: 11

# Task 12 — cli-output-renderers

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Three rendering paths (table, JSON, YAML) with NDJSON for streaming RPCs, plus a typed `Error` renderer used by every non-`serve` subcommand.

## Tasks
- [x] Create `internal/cliclient/output.go` with `RenderTable(w, headers, rows)` using `text/tabwriter` for columnar alignment, `RenderJSON(w, v)` with `json.MarshalIndent` two-space indent, `RenderYAML(w, v)` with `gopkg.in/yaml.v3` indent 2.
- [x] Table rules: cells longer than `maxCellWidth=40` are truncated and terminated with `…`; columns are space-padded via tabwriter. ANSI color is intentionally not enabled in v1 — TTY detection + theme color can land in a follow-up; the data still renders cleanly to non-TTY writers.
- [x] JSON: keys come out alphabetically for maps (Go encoding/json default); struct fields keep declaration order. AC verified via `jq '.|keys'` on a 4-key map fixture.
- [x] Add `RenderJSONStream(w, ch <-chan any)` writing one compact JSON object per line (NDJSON), flushing the writer when it supports Flush().
- [x] Create `internal/cliclient/errors.go` with `RenderError(w, *pb.Error)` rendering `Error: <code> — <message>` followed by alphabetically-sorted `<key>=<value>` detail lines. Plus `RenderConnectionError(w, addr, reason)` for the transport-layer "Connection error" message. `ExtractError(err)` peels a `pb.Error` out of a gRPC status's details or synthesizes one from the status code + message; callers exit non-zero themselves (avoids `os.Exit` inside a library — keeps the function testable).
- [x] Helper `SortedMapKeys[V any](m)` and `PrintableLines(s)` for the eventual CLI consumers.
- [x] Twelve unit tests in `internal/cliclient/output_test.go` + `helpers_grpc_test.go` cover RenderTable (headers + rows, truncation, no-headers path), RenderJSON (alphabetical map keys, 2-space indent), RenderYAML (round-trip via yaml.Unmarshal), RenderJSONStream (3-line NDJSON), RenderError (alphabetical detail order, nil-input fallback), RenderConnectionError (exact format), ExtractError (status without details synthesizes), and SortedMapKeys round-trip.
- [x] Migrating existing CLI subcommands (node / token / audit / backup / restore) to consume cliclient + renderers is **deferred** — the inline `dialServer` helper + printf-style output stays in place. Renderers are a standalone library; subcommand migration is a follow-up cleanup.

## Acceptance criteria
- [x] `go test ./internal/cliclient/... -race -count=1 -run Render` exits 0.
- [x] `jq '.|keys'` on the alphabetical-keys fixture returns `["alpha","another","middle","zeta"]`.
- [x] `git grep -nE 'NDJSON|RenderJSONStream' internal/cliclient/` matches.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
