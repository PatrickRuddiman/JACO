Parent slice: [scheduler](../slices/scheduler.md)
Depends on: 04, 14

# Task 20 — placement-and-counter

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Pure placement engine (spread / hosts / pack) + `ReplicaCounter` raft entity with monotonic-never-reuse semantics.

## Tasks
- [ ] Create `internal/scheduler/placement/placement.go` with `EligibleHosts(spec ServiceSpec, nodes []Node) []string` returning the candidate set (healthy nodes intersected with `spec.Hosts` if set; healthy nodes otherwise).
- [ ] Implement `PlaceReplica(spec ServiceSpec, eligible []string, replicaIndex int, currentReplicaCounts map[string]int) (host string, err error)`. Modes per scheduler slice §3:
  - `hosts: [...]`: if `len(eligible) < spec.Replicas`, return `Error{code:"cannot_satisfy_host_placement", details:{requested, eligible}}`.
  - `spread` (default): `eligible[fnv32(deployment+service+strconv.Itoa(replicaIndex)) % len(eligible)]`. Sort `eligible` lexicographically first for determinism.
  - `pack`: sort by `currentReplicaCounts[host]` ascending, tiebreak by hostname lex; pick first.
- [ ] Create `internal/scheduler/counter/counter.go` with `Next(ctx, deployment, service string, raftApply func(cmd []byte) error) (uint64, error)`. Submits `Command{ReplicaCounterIncrement}{deployment, service}` to raft; FSM mutates `ReplicaCounter{next_index}` and the response is the value used.
- [ ] Replica id format: `fmt.Sprintf("%s-%s-%d", deployment, service, index)`. Indices never reused after a replica is removed.
- [ ] Unit tests with table-driven cases per mode: hosts insufficient → error; spread determinism (same input → same host across multiple calls); pack with fragmented counts.
- [ ] Property test for spread mode: for any (deployment, service) with 3 eligible hosts and 9 replica indices, replicas distribute within 1 of uniform.

## Acceptance criteria
- [ ] `go test ./internal/scheduler/placement/... ./internal/scheduler/counter/... -race -count=1` exits 0.
- [ ] Test asserts `PlaceReplica` is deterministic across 1000 repeated calls with the same input.
- [ ] Test asserts an incremented counter is never reused across delete+recreate of the same service.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
