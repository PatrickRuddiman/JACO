Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 02, 01

# Task 03 — entity-stores-and-watch

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Implement typed in-memory entity stores and a generic per-entity-type pub/sub broker with bounded buffers, drop-newest overflow, and `Resync` signaling.

## Tasks
- [ ] Create `internal/controlplane/state/state.go` defining `type Store struct { Nodes, Deployments, ReplicasDesired, ReplicasObserved, Routes, Certs, ChallengeTokens, Tokens, JoinTokens, Subnets, RolloutPlans, ReplicaCounters, RestartCounters, AuditEvents, Cluster *<TypedStore> }`.
- [ ] Create one file per entity under `internal/controlplane/state/` (`nodes.go`, `deployments.go`, `replicas_desired.go`, `replicas_observed.go`, `routes.go`, `certs.go`, `challenge_tokens.go`, `tokens.go`, `join_tokens.go`, `subnets.go`, `rollout_plans.go`, `replica_counters.go`, `restart_counters.go`, `audit_events.go`, `cluster.go`). Each: `sync.RWMutex`, primary-key map, `Get`, `List`, `Apply<Mutation>` methods.
- [ ] Create `internal/controlplane/watch/broker.go` defining `type Broker[T any] struct { ... }` with `Subscribe(buf int) (<-chan Event[T], func() /*cancel*/)` and `Publish(Event[T])`. Buffer default 256. On send to a full subscriber channel: drop the oldest in-flight event and enqueue an `Event[T]{Kind: RESYNC}` so the subscriber re-fetches.
- [ ] Create `internal/controlplane/watch/registry.go` instantiating one `*Broker[T]` per entity type; exposed via getters.
- [ ] Create `internal/controlplane/watch/broker_test.go`: subscribe → publish 10 events → receive all 10 in order; a slow subscriber (never reads) gets a `RESYNC` event after 257+ publishes and no panic.
- [ ] Create `internal/controlplane/state/state_test.go`: insert/update/list round-trip per entity type.

## Acceptance criteria
- [ ] `go test ./internal/controlplane/state/... ./internal/controlplane/watch/... -race -count=1` exits 0.
- [ ] `git grep -nE 'type Broker\[' internal/controlplane/watch/broker.go` returns 1 match.
- [ ] `go vet ./internal/controlplane/state/... ./internal/controlplane/watch/...` exits 0.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
