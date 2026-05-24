Parent spec: [spec.md](../spec.md) · Design: [design.md](../design.md)

# JACO — control-plane

## §1 Summary

Replicated state machine and raft membership for the cluster. Owns command admission, leader-forwarded writes, the canonical entity store, watch fan-out to in-process subscribers, audit log writes, cluster CA, and snapshot/backup/restore. Every other vertical reads or writes the cluster's source of truth through this slice's gRPC surface.

## §2 Codebase reconnaissance

Greenfield: no existing system to reconcile. Decisions below are unconstrained.

## §3 Decisions

1. **Raft library.** Options: `hashicorp/raft`, `go.etcd.io/raft`, `lni/dragonboat`. **Chosen:** `hashicorp/raft` v1.7+. Rationale: bundled transport + FSM interface cuts ~1k lines of boilerplate; production analog (Nomad) uses it; quarterly releases.
2. **FSM apply-payload serialization.** Options: protobuf, gob, JSON. **Chosen:** protobuf, reusing the same proto definitions as the gRPC API. Rationale: one schema across wire and storage; backward-additive evolution; cheapest cross-version compatibility.
3. **Log/stable store backend.** Options: `raft-boltdb-v2`, `raft-badger`, in-memory. **Chosen:** `raft-boltdb-v2`. Rationale: official sister project, fsync semantics well-understood, what Consul/Nomad ship.
4. **Watch fan-out shape.** Options: per-entity-type pub/sub topics, single fan-out channel, direct callbacks. **Chosen:** per-entity-type pub/sub topics. Rationale: type-safe, bounded channels prevent slow watchers from stalling raft apply.
5. **Backup format.** Options: raft snapshot tarball + metadata, custom JSON dump, both. **Chosen:** raft snapshot tarball + metadata. Rationale: reuses the raft snapshot pipeline; restore primes a fresh raft store directly.

## §4 Contracts & shapes

Module layout under `internal/controlplane/`:

- `internal/controlplane/raft/` — hashicorp/raft wiring: `Node` struct holding the `*raft.Raft`, log store, snapshot store, transport (TCP transport from hashicorp/raft).
- `internal/controlplane/fsm/` — implements `raft.FSM`: `Apply(*raft.Log) interface{}`, `Snapshot() (FSMSnapshot, error)`, `Restore(io.ReadCloser) error`.
- `internal/controlplane/state/` — typed entity stores; one file per entity (`nodes.go`, `deployments.go`, `replicas_desired.go`, `replicas_observed.go`, `routes.go`, `certs.go`, `tokens.go`, `audit.go`). Each store owns a `sync.RWMutex` and an in-memory map keyed by primary id.
- `internal/controlplane/watch/` — pub/sub: one `*broker[T]` per entity type, generics over each typed Event. Buffered channels (default 256) with drop-newest-on-overflow; subscribers resume via raft log index.
- `internal/controlplane/admission/` — gRPC interceptor that resolves bearer tokens via `state.Tokens.Lookup(hash)`, attaches `identity` to context, rejects with `token_invalid` / `token_revoked`.
- `internal/controlplane/grpc/` — server-side handlers for `Cluster.*`, `Deploy.*`, `Audit.*`, `Token.*`, `Watch.Subscribe`. Write RPCs proxy to leader via `raft.Apply`; non-leader nodes forward via internal gRPC call to leader's address.
- `internal/controlplane/ca/` — cluster CA generation at bootstrap, node cert minting on first boot, join-token issuance + single-use validation.
- `internal/controlplane/backup/` — `Export(io.Writer)` snapshots raft and writes `tar.gz(snapshot.bin, meta.json)`; `Import(io.Reader)` validates meta and seeds a fresh raft store.

Apply command shape (single proto type with oneof per command):

- `Command { cluster_id, raft_index, ts, identity, oneof payload { NodeJoin, NodeRemove, DeploymentApply, DeploymentRollback, DeploymentDelete, ReplicaObservedUpdate, RouteUpdate, CertUpdate, TokenIssue, TokenRevoke } }`
- Each variant carries only the fields needed to mutate the entity store. FSM `Apply` switches on the variant and calls the matching `state.<entity>.Apply` method.

Watch event shape:

- `Event<T> { kind ∈ {Added, Updated, Removed}, before: T?, after: T?, raft_index }`. Subscribers receive a typed stream; the gRPC `Watch.Subscribe` handler converts to proto and writes to the client.

Backup tarball layout:

- `snapshot.bin` — raw raft snapshot bytes from `raft.Snapshot()`.
- `meta.json` — `{cluster_id, snapshot_index, snapshot_term, jaco_version, taken_at, leader_at_snapshot}`.

Cluster CA contract:

- CA cert + CA private key stored in raft state under a singleton `Cluster` entity.
- Per-node server cert + key live in `$JACO_DATA/node/{hostname}.{key,crt}`, generated at first boot, signed by the CA (a node fetches the CA private key through a raft read at bootstrap-or-join time, signs its own CSR locally, never persists the CA key outside raft).
- Join token: 32-byte random, hashed and stored under `JoinToken{hashed_secret, issued_at, expires_at, consumed_at?}` — single-use, 24h default expiry.

## §5 Sequence

Apply command (write path):

1. Any node's gRPC handler receives `Deploy.Apply(jaco_yaml)` with bearer token.
2. `admission` interceptor validates token, attaches identity.
3. Handler validates the jaco.yaml against the entity-type closed sets (returns `validation_failed` with details on rejection).
4. Handler builds a `Command{DeploymentApply}` proto, marshals.
5. Non-leader: forwards to leader via internal gRPC `Internal.Submit(Command)`; leader: directly calls `raft.Apply(bytes, timeout=5s)`.
6. Raft replicates to majority, applies to FSM on every node.
7. FSM `Apply` decodes, mutates `state.Deployments`, writes `AuditEvent{type: apply, identity, payload: diff_summary}`, publishes `Event<Deployment>{Updated}` to the deployments topic.
8. Watch subscribers (scheduler, cli `status -w`) receive the event from their topic broker and react.
9. Handler returns success (with the new revision) to the originating CLI.

Watch subscribe (read path):

1. Subscriber (scheduler, runtime, ingress, discovery, or `jaco status -w`) calls `Watch.Subscribe(entity_type, since_revision?)`.
2. Handler creates a typed subscription on the matching broker, with a buffered channel.
3. If `since_revision` provided, the handler first replays Events from the state snapshot iterating entries with `raft_index > since_revision` (catch-up phase).
4. After catch-up, handler streams new Events as they arrive in the channel.
5. If channel overflows (slow subscriber), the broker drops oldest event and emits a synthetic `Event{kind: Resync}` to signal the client to re-fetch full state.

Bootstrap:

1. `jaco bootstrap` on first node generates cluster id (UUID), cluster CA (RSA-4096 or Ed25519), node server cert.
2. Initializes raft with `BootstrapCluster=true`, single voter (self).
3. Applies seed `Command{ClusterInit}` carrying cluster id, CA cert+key, and a freshly minted operator token under identity `bootstrap`.
4. Prints the operator token once on stdout; never logged elsewhere.

Node join:

1. Operator runs `jaco node join --address <leader-or-any>:7000 --join-token <single-use> --name <hostname>`.
2. Joining node validates the join token's signature against the cluster CA cert (which it received with the token), generates its own server cert key locally, requests a CSR-sign via `Internal.SignNodeCert`.
3. Leader sees the join request, validates the join token (mark consumed), signs the CSR, returns the signed cert.
4. Leader issues `raft.AddVoter(node)`; FSM publishes `Event<Node>{Added}`.
5. Joining node opens raft connection, catches up snapshot+log, transitions follower.

Backup / restore:

1. `jaco backup --output cluster.tar.gz` triggers `raft.Snapshot()`, writes `snapshot.bin` + `meta.json` into a tarball.
2. `jaco restore --input cluster.tar.gz` validates `meta.json` against the running daemon's version compatibility, primes a fresh raft store from `snapshot.bin`, starts raft with `BootstrapCluster=true` on the receiving node only. Operator joins peers via `jaco node join` as usual.

## §6 Out of scope

- Specific scheduler reconcile logic (lives in scheduler slice).
- Specific runtime docker integration (lives in runtime slice).
- CLI output formatting (lives in cli slice).
- Cert renewal policy beyond storage (covered in ingress slice).

> If the parent spec is ambiguous on anything this slice depends on, stop and update the spec. Do not invent behavior here.
