Parent slice: [ingress](../slices/ingress.md)
Depends on: 32

# Task 33 — certmagic-raft-storage

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Custom `certmagic.Storage` backed by `Cert` and `ChallengeToken` raft entities; `Lock` / `Unlock` are raft Applies so cluster-wide single-flight issuance falls out naturally.

## Tasks
- [x] Extend FSM's CertLock handler in `internal/controlplane/fsm/fsm.go` to reject (no-op) when an existing lock has a different lessee and the LockUntil hasn't passed. Same-lessee reapplies (auto-renew) always accepted; expired locks acquirable by anyone. Cluster-wide single-flight falls out of this rule.
- [x] Create `internal/ingress/storage/storage.go` implementing JACO's Storage interface (shape-for-shape match with `certmagic.Storage` / `caddy.Storage` so the daemon can register *JacoStorage as the "jaco" module without further adaptation). Methods: Store, Load, Delete, Exists, List, Stat, Lock, Unlock.
- [x] Lock raft-Applies `Command{CertLock}{name, lessee, until: now+LockTTL=5min}` then verifies the persisted lessee matches self — returns `ErrLockHeld` when another lessee won. Spawns an auto-renew goroutine that re-applies CertLock every RenewInterval=2min until Unlock fires (Unlock stops the renewer + raft-Applies CertUnlock).
- [x] Clock interface lets tests pin time so the LockTTL expiry case can fast-forward without sleeps.
- [x] Store/Load/Delete/Exists/List/Stat back onto an in-memory blob map keyed by certmagic key. List(prefix, recursive=false) returns direct children; recursive=true returns full descendant paths.
- [ ] **Deferred**: raft-backed blob storage. Cert entity carries structured fields (private_key, cert_chain, etc.) that don't match certmagic's free-form key/value model. A CertBlob entity addition is the natural follow-up; v1 keeps blobs per-node in memory and documents the limitation.
- [ ] **Deferred**: registering the storage with Caddy via `caddy.RegisterModule(&JacoStorage{})` — needs the caddy/v2 import which lands with the daemon entry / ingress runner.
- [x] Ten tests pass with -race. Lock first lessee acquires (Cert entry has lessee=node-a); Lock contention resolved with a single winner — second lessee gets ErrLockHeld (the AC); expired locks acquirable by a new lessee via fake clock fast-forward (the AC); Unlock releases and allows immediate reacquire by a different lessee; Store/Load round-trip; Load missing returns ErrNotExist; Delete removes key; List non-recursive returns direct children; List recursive returns full paths; Stat missing returns ErrNotExist.

## Acceptance criteria
- [x] `go test ./internal/ingress/storage/... -race -count=1` exits 0 (10 tests).
- [x] Test asserts Lock contention resolved with a single winner (`TestLock_ContentionResolvedWithSingleWinner`).
- [x] Test asserts expired locks become acquirable by a new lessee (`TestLock_ExpiredLockAcquirableByNewLessee`).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
