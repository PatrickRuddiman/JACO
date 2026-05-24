Parent slice: [scheduler](../slices/scheduler.md)
Depends on: 04, 14

# Task 20 — placement-and-counter

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Pure placement engine (spread / hosts / pack) + `ReplicaCounter` raft entity with monotonic-never-reuse semantics.

## Tasks
- [x] Create `internal/scheduler/placement/placement.go` with `EligibleHosts(spec, nodes)` returning healthy nodes intersected with `spec.Hosts` (HOSTS mode only); always sorted for determinism.
- [x] Implement `PlaceReplica(deployment, spec, eligible, replicaIndex, currentReplicaCounts)`:
  - SPREAD (default): `eligible[fnv32(deployment-service-replicaIndex) mod len(eligible)]`. `eligible` sorted before hashing so input order doesn't matter.
  - PACK: sort eligible by `currentReplicaCounts[host]` ascending with hostname-lex tiebreak; pick first.
  - HOSTS: if `len(eligible) < spec.Replicas` return `PlacementError{code:cannot_satisfy_host_placement, details:{requested, eligible}}`; otherwise round-robin by replicaIndex over the sorted eligible set.
- [x] Create `internal/scheduler/counter/counter.go` with `Counter.Next(deployment, service)`. Reads current value from `state.ReplicaCounters`, raft-Applies `Command{ReplicaCounterIncrement}` through an injected `Applier` so the FSM bumps `next_index`, returns the new index. `ReplicaID(deployment, service, index)` is the canonical formatter `<deployment>-<service>-<index>`.
- [x] Eleven placement tests pass with -race: EligibleHosts filters by READY status; HOSTS-mode intersects with spec.Hosts; no eligible hosts → cannot_satisfy_host_placement; HOSTS insufficient → cannot_satisfy_host_placement with requested + eligible details; HOSTS round-robins one replica per pinned host; SPREAD deterministic across 1000 calls (the AC); SPREAD distributes 9 replicas across 3 hosts within 1 of uniform; SPREAD stable under eligible-ordering shuffle; PACK picks least-loaded; PACK tiebreaks by hostname lex; PACK treats missing counts as 0.
- [x] Six counter tests pass with -race: starts at 1; monotonic across 100 calls without duplicates; never reuses indices across DeploymentDelete + recreate (the AC) — verified by issuing 3 ids, applying DeploymentDelete, and asserting Next() returns 4 (not 1); per-service counters are independent; empty args rejected; ReplicaID format `<dep>-<svc>-<index>`.

## Acceptance criteria
- [x] `go test ./internal/scheduler/placement/... ./internal/scheduler/counter/... -race -count=1` exits 0.
- [x] Test asserts PlaceReplica is deterministic across 1000 repeated calls with the same input (`TestPlaceReplica_SpreadDeterministic1000Calls`).
- [x] Test asserts an incremented counter is never reused across delete+recreate of the same service (`TestNext_NeverReusesAfterDeploymentDelete`).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
