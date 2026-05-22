Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 03

# Task 04 — fsm-apply

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Implement `raft.FSM` that decodes `Command` proto, mutates the entity Store, writes an audit event, and publishes the matching typed `Event` through the per-entity-type broker.

## Tasks
- [x] Create `internal/controlplane/fsm/fsm.go` defining `type FSM struct { State *state.State; Brokers *watch.Registry }` implementing `hraft.FSM`: `Apply(*hraft.Log) interface{}`, `Snapshot() (hraft.FSMSnapshot, error)`, `Restore(io.ReadCloser) error`. (Field is `State *state.State` since the composite struct was renamed in task 03.)
- [x] In `Apply`: `proto.Unmarshal` bytes to `pb.Command`, switch over `Command.Payload` oneof, dispatch to the matching state mutation (which fans the watch event out via the stored broker), append an `AuditEvent` for the closed set of user-visible mutations (`identity` from `Command.identity`, `raft_index` from `log.Index`).
- [x] Define `type fsmSnapshot struct { data []byte }` with `Persist(sink hraft.SnapshotSink) error` writing the proto-encoded full State; `Release()` no-op. Snapshot proto added at `proto/jaco/v1/fsm.proto` (`FSMSnapshot` with one repeated field per entity type).
- [x] `Restore` reads the proto-encoded snapshot and re-applies every entity at raft index 0.
- [x] Stub variants whose real interpretation belongs to later tasks: `DeploymentRollback` (revision restore: task 14), `CertStore` key/value namespacing (task 33), `ReplicaCommandIssue` ledger (task 23). Each is a documented no-op in the switch.
- [x] Create `internal/controlplane/fsm/fsm_test.go`: build a `pb.Command{NodeJoin}`, marshal, feed to `FSM.Apply` via a synthesized `*hraft.Log`; assert state contains the new node, a subscriber receives `Event[Node]{Kind: Added}`, and `AuditEvents.List()` contains 1 entry with `type=node_join` and `raft_index` populated. Plus token issue/revoke round-trip, deployment apply→delete cascade on routes, replica counter increment, batch recursion, unmarshal error path, and snapshot/restore round-trip.

## Acceptance criteria
- [x] `go test ./internal/controlplane/fsm/... -race -count=1` exits 0.
- [x] Test asserts the new audit event and the broker event are both written.
- [x] `git grep -nE 'func \(.+ \*FSM\) Apply\(.+\*(hraft|raft)\.Log\)' internal/controlplane/fsm/fsm.go` returns exactly 1 match.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
