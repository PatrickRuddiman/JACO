Parent slice: [ingress](../slices/ingress.md)
Depends on: 32

# Task 33 — certmagic-raft-storage

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Custom `certmagic.Storage` backed by `Cert` and `ChallengeToken` raft entities; `Lock` / `Unlock` are raft Applies so cluster-wide single-flight issuance falls out naturally.

## Tasks
- [ ] Create `internal/ingress/storage/storage.go` implementing the `certmagic.Storage` interface: `Store, Load, Delete, Exists, List, Stat, Lock, Unlock`.
- [ ] `Store(key, value)`: raft-Apply `Command{CertStore}{key, value, ttl_seconds}` for `cert/<domain>/*` keys (mapped to `Cert` entity) and `challenge/<token>` keys (mapped to `ChallengeToken` entity).
- [ ] `Load(key)`: read from local watch-fed cache (`state.Certs`, `state.ChallengeTokens`); on miss with watch reporting `Resync` in progress, perform a leader-read via `Internal.Submit` echo of a no-op then re-read locally.
- [ ] `Delete, Exists, List, Stat`: straightforward over the local cache; `List(prefix, recursive=false)` enumerates direct children of the prefix.
- [ ] `Lock(name)`: raft-Apply `Command{CertLock}{name, lessee:<node>, until: now+5min}`. If a non-expired lock exists with a different lessee, return `certmagic.ErrLockExpired`-like error. Auto-renew: spawn a goroutine that re-applies `CertLock` every 2 minutes while held.
- [ ] `Unlock(name)`: raft-Apply `Command{CertUnlock}{name}`.
- [ ] Register the storage with certmagic in `internal/ingress/ingress.go::init`: `caddy.RegisterModule(&JacoStorage{})` with module ID `caddy.storage.jaco`.
- [ ] Unit tests in `internal/ingress/storage/storage_test.go`: two `Lock(name)` calls from different lessees — one succeeds, the other returns the contention error. Lock expiry: after 5min + 1s (simulated via fake clock), a new lessee can take the lock. Store/Load round-trip through a fake raft.

## Acceptance criteria
- [ ] `go test ./internal/ingress/storage/... -race -count=1` exits 0.
- [ ] Test asserts Lock contention is resolved with a single winner.
- [ ] Test asserts expired locks become acquirable by a new lessee.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
