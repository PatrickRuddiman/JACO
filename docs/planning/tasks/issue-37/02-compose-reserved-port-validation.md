Parent slice: [TCP ingress — control-plane](../../slices/issue-37/control-plane.md)
Depends on: none

# Task 02 — Compose reserved-port (80/443) validation

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`compose.Validate` rejects any service `ports:` entry that publishes host port 80 or 443 (short, long, or range syntax) with a typed `reserved_port` error, while leaving container-side and bare/documentation ports untouched.

## Tasks
- [ ] In `internal/runtime/compose/validate.go`, inside the per-service loop (`validate.go:91-122`, after the `networks:` check), inspect the raw `ports:` field of each service. The field is a YAML list whose items are either strings or maps; parse the **published host side** only.
- [ ] Parse string forms: `"H:C"`, `"H1-H2:C1-C2"` (range), `"IP:H:C"` (strip the IP), and bare `"H"` (no colon → no published host side). Parse long-form map items via the `published`/`target` keys.
- [ ] If a parsed published value equals `80` or `443`, or a published range covers either, return `&ValidationError{Code: "reserved_port", Message: fmt.Sprintf("service %q publishes reserved host port %s (entry %q); 80 and 443 belong to JACO's HTTP/S ingress", svcName, port, raw), Details: map[string]string{"service": svcName, "port": port, "entry": raw}}`.
- [ ] Do **not** flag: container/target-side `80`/`443` (e.g. `"8080:80"`), bare entries with no published host side (`"80"`), and non-TCP/`127.0.0.1`-scoped published sides are out of this check's remit (still rejected only if the published number is 80/443). First violation wins; the service loop is already sorted (`validate.go:84-89`) so the first offender is deterministic.
- [ ] Create `internal/runtime/compose/validate_test.go` with inline-YAML cases (mirror the byte-input style of `compose_test.go`).

## Acceptance criteria
- [ ] `go test ./internal/runtime/compose/ -run Validate` passes with these cases: `"80:80"` → `reserved_port`; `"443:443"` → `reserved_port`; long `{published: 80, target: 80}` → `reserved_port`; range `"79-81:79-81"` → `reserved_port`; `"8080:80"` (container-side 80) → no error; bare `"80"` → no error; `"5432:5432"` → no error.
- [ ] The test asserts the returned error's `Code == "reserved_port"` and that `Message`/`Details` name the offending service and entry.
- [ ] `go build ./...` exits 0.
- [ ] `test -f internal/runtime/compose/validate_test.go`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
