Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 03

# Task 04 — fsm-apply

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Implement `raft.FSM` that decodes `Command` proto, mutates the entity Store, writes an audit event, and publishes the matching typed `Event` through the per-entity-type broker.

## Tasks
- [ ] Create `internal/controlplane/fsm/fsm.go` defining `type FSM struct { Store *state.Store; Brokers *watch.Registry }` implementing `raft.FSM`: `Apply(*raft.Log) interface{}`, `Snapshot() (raft.FSMSnapshot, error)`, `Restore(io.ReadCloser) error`.
- [ ] In `Apply`: `proto.Unmarshal` bytes to `pb.Command`, switch over `Command.Payload` oneof, dispatch to `Store.<Entity>.Apply<Mutation>(payload)`, append an `AuditEvent` describing the mutation (`identity` from `Command.identity`), then call the matching `Brokers.<entity>().Publish(Event[T]{Kind, Before, After, RaftIndex: log.Index})`.
- [ ] Define `type fsmSnapshot struct { data []byte }` with `Persist(sink raft.SnapshotSink) error` writing the proto-encoded full Store; `Release()` no-op.
- [ ] `Restore` reads the proto-encoded snapshot and replaces `Store`'s contents atomically.
- [ ] Create `internal/controlplane/fsm/fsm_test.go`: build a `pb.Command{NodeJoin}`, marshal, feed to `FSM.Apply` via a synthesized `*raft.Log`; assert (a) `Store.Nodes` contains the new node; (b) a subscriber on `Brokers.Nodes()` receives `Event[Node]{Kind: ADDED}`; (c) `Store.AuditEvents.List()` contains 1 entry with `type=node_join`.

## Acceptance criteria
- [ ] `go test ./internal/controlplane/fsm/... -race -count=1` exits 0.
- [ ] Test asserts the new audit event and the broker event are both written.
- [ ] `git grep -nE 'func \(.+ \*FSM\) Apply\(.+\*raft\.Log\)' internal/controlplane/fsm/fsm.go` returns exactly 1 match.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
