Parent slice: [TCP ingress — datapath](../../slices/issue-37/datapath.md)
Depends on: 00, 04

# Task 05 — Daemon TCP projection + bind-probe + loader gate + reload subscription

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Wire the daemon's ingress builder to project `state.TCPRoutes` into `BuildCaddyConfig` (skipping ports that can't bind on this node), make the embedded loader load layer4-only configs, and fire the rebuild loop on `TCPRoute` changes.

## Tasks
- [x] In `internal/daemon/grpc/ingress.go` `ingressBuilder` (`ingress.go:33`), gather a `[]config.TCPRoute` from `st.TCPRoutes.List()`: for each `TCPRoute`, set `PublishedPort`/`Deployment`/`Service`/`ContainerPort` and reuse the per-service overlay-IP/eligible-replica data already computed for HTTP (`ingress.go:66-89`).
- [x] Bind-probe each candidate published port: `net.Listen("tcp", ":<port>")` then immediately `Close()`; on error, drop that `TCPRoute` from the set and log `ingress: tcp port <port> unbindable on this node, skipping (degraded): <err>`.
- [x] Pass the surviving `[]config.TCPRoute` into `BuildCaddyConfig`, replacing the `nil` placeholder added in task 04 (`ingress.go:91`).
- [x] In `ingressLoaderEmbedded` (`ingress.go:117-120`), extend the "has a real route" guard so it loads when the config contains `"reverse_proxy"` **or** a layer4 `"proxy"` handler. Extract the predicate into a small named helper (e.g. `configHasLoadableRoute([]byte) bool`) so it is unit-testable.
- [x] In `internal/ingress/rebuild/rebuild.go` `Run` (`rebuild.go:77-115`), add `r.brokers.TCPRoutes.Subscribe()` and a corresponding `case` in the select loop that sets `pending` + resets the debounce timer, identical to the `routes` case (`rebuild.go:97-99`).
- [x] Add tests: `internal/ingress/rebuild/rebuild_test.go` (a `TCPRoutes` event triggers a rebuild); `internal/daemon/grpc/` ingress tests for the bind-probe (occupy a port via `net.Listen`, assert the built config omits that `tcp_<port>` server) and the loader-gate predicate (a layer4-only config returns true).

## Acceptance criteria
- [x] `go test ./internal/ingress/rebuild/...` passes, including a case where publishing a `TCPRoute` event causes exactly one additional rebuild.
- [x] `go test ./internal/daemon/grpc/ -run 'Ingress|TCP|BindProbe|LoaderGate'` passes: an occupied port is excluded from the built config; `configHasLoadableRoute` returns true for a layer4-only config and false for a fallback-404-only config.
- [x] `go build ./...` exits 0.
- [x] `git grep -nE 'TCPRoutes\.Subscribe' internal/ingress/rebuild/rebuild.go` matches and `git grep -nE 'net\.Listen\("tcp"' internal/daemon/grpc/ingress.go` matches.

## Post-cluster-test revision (2026-05-25)
Live 3-node testing surfaced two defects in the as-shipped task, both fixed in a follow-up commit (slice §3 decision 5 + §4 loader gate updated to match):
- **Bind-probe removed.** `net.Listen(":port")` false-positives on caddy-l4's *own* listener, so the route was dropped on every rebuild, flapping the listener. The builder now emits every route; `caddy.Load` is an idempotent graceful swap for ports it already owns. A genuine foreign conflict fails the atomic load and the Reloader keeps last-good.
- **Route-less configs now load once caddy is running** (`shouldLoad(started, cfg)`), so deleting the last route tears its listeners down. The skip applies only before caddy's first start. Without this, deleted TCP listeners lingered cluster-wide.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
