Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 00

# Task 01 — proto-definitions

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Define the protobuf schema (entities, commands, events, error envelope, gRPC services) and wire `buf`-based code generation into `pkg/proto/v1/`.

## Tasks
- [x] Create `proto/jaco/v1/entities.proto` with messages: `Node`, `Deployment`, `ServiceSpec`, `ReplicaDesired`, `ReplicaObserved`, `Route`, `Cert`, `ChallengeToken`, `Token`, `JoinToken`, `AuditEvent`, `Subnet`, `RolloutPlan`, `ReplicaCounter`, `RestartCounter`, `ClusterMeta`. Enums for closed sets: `ReplicaState`, `RolloutState`, `AuditEventType`, `NodeStatus`, `DeploymentStatus`.
- [x] Create `proto/jaco/v1/commands.proto` with `message Command { string cluster_id; uint64 raft_index; google.protobuf.Timestamp ts; string identity; oneof payload { ClusterInit, NodeJoin, NodeRemove, NodeStatusUpdate, DeploymentApply, DeploymentRollback, DeploymentDelete, DeploymentStatusUpdate, ReplicaDesiredUpsert, ReplicaDesiredRemove, ReplicaObservedUpdate, ReplicaCommandIssue, RolloutPlanUpdate, ReplicaCounterIncrement, RouteUpsert, RouteRemove, CertStore, CertLock, CertUnlock, ChallengeTokenStore, SubnetAllocate, SubnetFree, TokenIssue, TokenRevoke, JoinTokenIssue, JoinTokenConsume, AuditAppend, Batch } }`.
- [x] Create `proto/jaco/v1/events.proto` with `enum EventKind { ADDED; UPDATED; REMOVED; RESYNC; }` and one `Event<Entity>` message per entity, each carrying `kind, before, after, raft_index`. (Audit-log Event is named `AuditLogEvent` to avoid collision with the `AuditEvent` entity.)
- [x] Create `proto/jaco/v1/error.proto` with `message Error { string code = 1; string message = 2; map<string,string> details = 3; }`.
- [x] Create `proto/jaco/v1/services.proto` defining gRPC `service Cluster { Bootstrap, IssueJoinToken, NodeJoin, NodeRemove, NodeList, Backup, Restore, Status }`, `service Deploy { Apply, Rollback, Delete, Status, Logs }`, `service Tokens { Issue, Revoke, List }` (renamed from `Token` to avoid collision with the entity message), `service Audit { Query }`, `service Watch { Subscribe }`, `service Internal { Submit, SignNodeCert, Logs }`.
- [x] Create `buf.yaml` (v2 config) and `buf.gen.yaml` generating Go + gRPC into `pkg/proto/jaco/v1/` (mirrors the source tree via `paths=source_relative`).
- [x] Wire `make proto` to `buf generate`.

## Acceptance criteria
- [x] `test -f proto/jaco/v1/entities.proto && test -f proto/jaco/v1/commands.proto && test -f proto/jaco/v1/services.proto && test -f proto/jaco/v1/events.proto && test -f proto/jaco/v1/error.proto`.
- [x] `buf lint` exits 0.
- [x] `make proto` exits 0; afterwards `test -f pkg/proto/jaco/v1/entities.pb.go && test -f pkg/proto/jaco/v1/services_grpc.pb.go`.
- [x] `go build ./pkg/proto/jaco/v1/...` exits 0.
- [x] `git grep -nE 'oneof payload' proto/jaco/v1/commands.proto` returns at least 1 match.
- [x] `go test ./pkg/proto/jaco/v1/... -count=1` exits 0.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
