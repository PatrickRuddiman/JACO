# ADR 0004: External-key encryption for replicated application state

- **Status:** proposed
- **Date:** 2026-09-19
- **Issue:** N/A; security audit finding 2 at `fbbea333bf04c93ad7b35610dc60a8b6493983b0` (not GitHub issue 2)

## Context

Resolved deployment manifests, registry credentials, the cluster CA signing
key and ACME private blobs enter the shared FSM. Plain Bolt logs, snapshots,
backup archives and certificate caches expose the same authority. Encrypting
only a new named-secret store would leave existing deployment and signing
paths unprotected. Old free pages and historical exports also survive a
forward-only format change.

The coordinator approved an independently provisioned file/service-manager
keyring as the conservative reviewable implementation policy. This records a
design decision, not authorization to rotate credentials, migrate live
clusters, delete history, or incur downtime.

## Decision

Require an external versioned 256-bit keyring, identical on every member and
outside application state/backups. Authenticate logical commands normally,
then encrypt before Raft. Use fresh per-envelope data keys with AES-256-GCM.
Authenticate persistent log replay metadata, snapshot metadata, backup
metadata and cache names. Decrypt only inside trusted processes. Redact
routine deployment views and require an explicit scoped operator export.

Startup and enrollment fail closed on missing/mismatched keys, unconverted
history or failed authentication. A cryptographic invariant breach inside
FSM Apply must stop the process: Hashicorp Raft otherwise advances its
applied index even when Apply returns an error object. Legacy upgrades and
keyring changes require a coordinated offline fresh copy preserving the
original artifacts. Backups are independently re-encrypted and drill-restored
before recovery keys or original media are retired.

## Alternatives considered

**Named secret references only:** useful as a future authoring interface, but
does not protect existing resolved manifests, CA authority, registry secrets,
ACME state or history. It is not a complete remediation.

**KMS/HSM-backed wrapping:** can improve external key custody and auditability,
but requires provider selection, identities, availability/recovery policy and
additional integration. The file/service-credential boundary can later host
such provisioning without embedding a wrapping key in exports.

**Manual passphrase unlock:** avoids an unattended external file but changes
restart and failover requirements. Not selected for this daemon.

**In-place/forward-only encryption:** leaves plaintext in old log values,
freed database pages, snapshots, caches and historical backups. Rejected.

## Consequences

No default/automatically distributed key exists. Existing clusters require a
maintenance window; mixed-version or rolling keyring changes are unsupported.
Unattended restart depends on independently available credentials. Key loss
can make encrypted history unrecoverable. The packaged non-root service
needs service-manager credentials or a directly readable owner-only file.

Every live member still holds all secrets and CA signing authority. This is
artifact confidentiality/integrity, not a Byzantine-member defense, memory
isolation, CA revocation, anti-rollback ledger, workload-volume encryption or
substitute for RPC authorization and peer transport TLS. Local node/WireGuard
identities retain their existing permission-protected format.

See [state encryption and migration](../operations/state-encryption.md) for
provisioning, retention, rotation, recovery and client-compatibility procedures.
