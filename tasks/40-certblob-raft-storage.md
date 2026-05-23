Parent slice: [ingress](../slices/ingress.md)
Depends on: 33, 34, 38

# Task 40 — certblob-raft-storage

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Replace `internal/ingress/storage/storage.go`'s in-memory blob map with a raft-replicated `CertBlob` entity so every node sees the same cert+key payloads CertMagic writes. Plus the `CERTIFICATE_RENEWED` / `CERTIFICATE_FAILED` audit emission `34-acme-issuance-and-rebuild` deferred.

## Tasks
- [ ] `proto/jaco/v1/entities.proto`: add `message CertBlob { string key = 1; bytes value = 2; google.protobuf.Timestamp updated_at = 3; }`. The free-form `key` is whatever CertMagic hands us (`certificates/<domain>/<file>`); `value` is the opaque blob.
- [ ] `proto/jaco/v1/commands.proto`: add `message CertBlobUpsert { CertBlob blob = 1; }` and `message CertBlobRemove { string key = 1; }`. Wire both into the `Command.payload` oneof.
- [ ] `make proto`: regenerate `pkg/proto/jaco/v1/*.pb.go` so the new types land.
- [ ] `internal/controlplane/watch/registry.go`: add `CertBlobs *Broker[*pb.CertBlob]` alongside the existing brokers.
- [ ] `internal/controlplane/state/`: new file `cert_blobs.go` with `state.CertBlobs *Store[*pb.CertBlob]` keyed by `blob.Key`. Wire into `state.New` next to `Certs`.
- [ ] `internal/controlplane/fsm/fsm.go`: handle `Command_CertBlobUpsert` → `state.CertBlobs.Apply` and `Command_CertBlobRemove` → `state.CertBlobs.Remove`. Audit type can reuse `AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED` when the upsert is keyed under `certificates/` and `_FAILED` is left to the OnEvent path below.
- [ ] `internal/ingress/storage/storage.go`: replace the in-memory `blobs` map with calls to `state.CertBlobs` + a `RaftApplier` injected at construction (the daemon passes `node.Apply`). `Store` raft-Applies `CertBlobUpsert`; `Delete` raft-Applies `CertBlobRemove`; `Load` / `Exists` / `Stat` / `List` read from the local watch-driven `state.CertBlobs.List`. The existing `Lock` / `Unlock` semantics on the `Cert` entity stay unchanged.
- [ ] `internal/ingress/storage/storage_test.go`: refresh the ten tests so the in-memory backend becomes an FSM-replaying fixture (apply commands through a real `fsm.FSM` so `state.CertBlobs` is populated the same way production does). Keep the existing `TestLock_*` cases against the real `Cert` entity.
- [ ] `internal/daemon/grpc/server.go:startSubsystems`: when `caddyAvailable()`, build the `storage.JacoStorage` with the daemon's `node.Apply` as the applier so blob writes replicate cluster-wide. Pass it through `rebuild.New` (the Reloader currently takes `Builder`+`Loader` only — extend with an optional `Storage` field or have the Loader carry a reference, whichever lands cleanest).
- [ ] `internal/ingress/challenge/challenge.go`: emit `AuditAppend{type:CERTIFICATE_RENEWED, payload:{domain}}` when `Issuer.Issue` succeeds, and `AuditAppend{type:CERTIFICATE_FAILED, payload:{domain, reason}}` when it fails — this is the CertMagic `OnEvent` audit pair from task 34's deferred list.

## Acceptance criteria
- [ ] `make proto && go build ./...` exits 0.
- [ ] `go test ./internal/ingress/storage/... -race -count=1` exits 0.
- [ ] `go test ./internal/controlplane/fsm/... -race -count=1` exits 0 (FSM handles the new commands).
- [ ] `git grep -nE 'CertBlob' pkg/proto/jaco/v1/` matches.
- [ ] `git grep -nE 'state\.CertBlobs' internal/` returns ≥3 hits (state, fsm, storage).
- [ ] `git grep -nE 'AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED|AUDIT_EVENT_TYPE_CERTIFICATE_FAILED' internal/ingress/challenge/` matches.
- [ ] `go test ./... -race -count=1` exits 0 across the whole tree.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
