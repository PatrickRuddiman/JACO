Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 02, 01

# Task 03 — entity-stores-and-watch

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Implement typed in-memory entity stores and a generic per-entity-type pub/sub broker with bounded buffers, drop-newest overflow, and `Resync` signaling.

## Tasks
- [x] Create `internal/controlplane/state/state.go` defining `type State struct { Nodes, Deployments, ReplicasDesired, ReplicasObserved, Routes, Certs, ChallengeTokens, Tokens, JoinTokens, Subnets, RolloutPlans, ReplicaCounters, RestartCounters, AuditEvents, Cluster *<TypedStore> }`. (Renamed from `Store` to avoid collision with the generic `Store[T]`.)
- [x] Create one file per entity under `internal/controlplane/state/` (`nodes.go`, `deployments.go`, `replicas_desired.go`, `replicas_observed.go`, `routes.go`, `certs.go`, `challenge_tokens.go`, `tokens.go`, `join_tokens.go`, `subnets.go`, `rollout_plans.go`, `replica_counters.go`, `restart_counters.go`, `audit_events.go`, `cluster.go`). Keyed entities share the generic `Store[T]` in `store.go` (Get/List/Apply/Remove with proto.Clone defensive copies); each entity file supplies its keyFn. AuditEvents (append-only) and Cluster (singleton) get their own struct types.
- [x] Create `internal/controlplane/watch/broker.go` defining `type Broker[T any] struct { ... }` with `Subscribe() *Subscription[T]` (channel + idempotent Cancel) and `Publish(Event[T])`. Buffer default 256. On send to a full subscriber channel: drop the oldest in-flight event and enqueue an `Event[T]{Kind: KindResync}` so the subscriber re-fetches.
- [x] Create `internal/controlplane/watch/registry.go` instantiating one `*Broker[T]` per entity type; exposed as struct fields on `*Registry`.
- [x] Create `internal/controlplane/watch/broker_test.go`: subscribe → publish 10 events → receive all 10 in order; a slow subscriber that never reads gets a Resync event after overflow; Cancel is idempotent; concurrent Publish does not deadlock; zero buffer falls back to `DefaultBuffer`.
- [x] Create `internal/controlplane/state/state_test.go`: insert/update/list round-trip on the generic store via Nodes; defensive-copy guarantee; Remove returns existed; Apply emits Added/Updated/Removed watch events with raft_index; Subnets composite-key helper; JoinTokens hex-encoded key; AuditEvents append-and-list; Cluster singleton round-trip.

## Acceptance criteria
- [x] `go test ./internal/controlplane/state/... ./internal/controlplane/watch/... -race -count=1` exits 0.
- [x] `git grep -nE 'type Broker\[' internal/controlplane/watch/broker.go` returns 1 match.
- [x] `go vet ./internal/controlplane/state/... ./internal/controlplane/watch/...` exits 0.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
