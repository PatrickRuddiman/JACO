---
sources:
  - internal/controlplane/raft/
  - cmd/jaco/self_upgrade.go
  - internal/packaging/
  - .github/workflows/release.yml
---

# Upgrades

JACO is normally upgraded **one node at a time** with `jaco self-upgrade`. The
command verifies the release tarball (minisign signature over
`SHA256SUMS`, plus SHA-256 of the tarball), atomically swaps both
binaries, restarts the daemon under systemd, and rolls back on health
failure.

CLI reference: [`jaco self-upgrade`](../cli/self-upgrade.md).

**Exception: the first upgrade from plaintext Raft to mandatory Raft TLS
requires a coordinated cutover of every peer.** Do not use the ordinary
rolling walkthrough for that transition. See
[Plaintext-to-TLS Raft cutover](#plaintext-to-tls-raft-cutover).

## Why one node at a time

JACO's design commits to a **single static binary per node** plus a
strict version-skew bound: an N+1 CLI must accept commands against an
N daemon within the same major version. gRPC field additions are
backward-compatible; raft FSM apply is binary-compatible across
adjacent versions. So a rolling upgrade — one node up, wait for
raft rejoin, move to the next — is the canonical path.

Cluster-wide coordinated upgrade (`jaco cluster upgrade --all`) is
explicitly **not** in v1; the operator drives the rotation.

## Walkthrough

For a cluster `{node-1, node-2, node-3}` going from `v0.1.0` to
`v0.2.0`:

### 1. Pick the new release

Confirm the artifact exists at the GitHub release page:

```
https://github.com/PatrickRuddiman/JACO/releases/download/v0.2.0/jaco-v0.2.0-linux-amd64.tar.gz
https://github.com/PatrickRuddiman/JACO/releases/download/v0.2.0/SHA256SUMS
https://github.com/PatrickRuddiman/JACO/releases/download/v0.2.0/SHA256SUMS.minisig
```

`self-upgrade` fetches all three automatically from the `--url` you
pass (the sibling URL pattern replaces the last path segment).

### 2. Upgrade the first node

```sh
ssh node-1
sudo jaco self-upgrade \
  --url https://github.com/PatrickRuddiman/JACO/releases/download/v0.2.0/jaco-v0.2.0-linux-amd64.tar.gz
```

The command:

1. Downloads tarball + checksums + signature.
2. Verifies the minisign signature against the embedded public key
   (`internal/packaging/release-pubkey.txt`). Any signature mismatch
   aborts before touching binaries.
3. Verifies the SHA-256 of the tarball against `SHA256SUMS`.
4. Extracts `jaco` and `jacod` from the tarball.
5. Saves `/usr/local/bin/jaco` and `/usr/local/bin/jacod` as
   `.prev`.
6. Stages new binaries as `.upgrading`, then atomically renames over
   the live paths.
7. Runs `systemctl restart jacod`.
8. Polls `/usr/local/bin/jacod --version` for up to 3 s.

On health-poll failure, both binaries are restored from `.prev`, the
daemon is restarted again, and the command exits non-zero with
`post-upgrade health check failed; rolled back`.

### 3. Confirm the upgrade

From any other node:

```sh
jaco node list --server $LEADER
```

Wait for `node-1` to appear back as a member (status `READY`). Then
on `node-1` itself:

```sh
jacod --version
jaco cluster status
```

### 4. Advance to the next node

Repeat for `node-2`, then `node-3`. The cluster maintains majority
throughout — a 3-node cluster tolerates one node down — so apply,
status, logs all keep working from the surviving nodes.

## Rolling back a release

A `self-upgrade` that **succeeds** does not preserve `.prev` forever —
the next `self-upgrade` overwrites them. To roll an upgraded cluster
back to the prior version, run `jaco self-upgrade` against the
prior version's tarball URL on each node.

A `self-upgrade` that **fails the post-restart health check** rolls
back automatically on that node only.

## On hosts without systemd

The restart step is a soft skip: binaries swap, but starting the new
daemon is the operator's job (`rc-service jacod restart`, manual
respawn, etc.). On Alpine in particular, JACO ships the `.apk` but
relies on the operator to wire whatever supervision they prefer.

## Verification keys

The minisign public key is embedded at build time from
[`internal/packaging/release-pubkey.txt`](../../internal/packaging/release-pubkey.txt).
Rotation requires:

1. Generate a new minisign keypair offline.
2. Publish a new JACO release whose pubkey constant is the new key,
   still signed with the **old** key (operators are running old code).
3. Operators upgrade to that release; their daemons now verify future
   releases with the new key.
4. The next release after that is signed with the new key.

See [release and packaging](../contributing/release-and-packaging.md).

## Plaintext-to-TLS Raft cutover

Plaintext Raft and TLS Raft are wire-incompatible. There is no automatic
downgrade, mixed-mode listener, self-signed Raft bootstrap identity, or
shared static transport key. TLS-to-TLS upgrades can continue to roll
normally after this one-time transition.

This is an operator-run maintenance procedure, not an automatic migration:

1. Inventory every voter and nonvoter, its Raft ID/address, and the matching
   `data_dir/node/<hostname>.crt`, `.key`, and `ca.crt`. Verify the
   certificates belong to the intended cluster, are valid, and allow
   server and client authentication. Retain a protected, verified backup
   and use a trusted management channel to stage the signed release.
2. Pause control-plane writes and stop `jacod` on **all** peers. Expect a
   control-plane availability interruption; schedule it explicitly.
3. Install the TLS-capable release on all peers while keeping their
   daemons stopped. Preserve Raft logs, snapshots, node identities, and
   addresses. Do not bootstrap new clusters over existing stores.
4. Start the upgraded peers, restore a majority, and verify leader
   election, every follower's applied index/catch-up, and a new replicated
   write before resuming changes.
5. If a node reports `raft TLS`, repair its trusted certificate/CA files
   or identity mismatch. Do not bypass verification. A rollback across
   this boundary must also be coordinated across the cluster and knowingly
   reintroduces the old plaintext exposure; never automatically downgrade
   individual peers to restore connectivity.

The ordinary one-at-a-time `self-upgrade` health check is not a mixed-mode
compatibility mechanism and must not be relied on for this cutover.

## Raft node certificates

For routine leaf rotation, issue a new per-node keypair and certificate
under the same trusted cluster CA, keeping the Raft ID in both common
name and SANs and retaining server/client authentication usages. In a
TLS-only cluster, stop one node while preserving quorum, replace its
matching `.crt`/`.key` files, protect the private key with mode `0600`,
then restart it and verify catch-up before advancing to the next node.
Keep `ca.crt` unchanged for a leaf-only rotation.

New Raft handshakes pick up replacement files, but stopping/restarting
also retires already-authenticated pooled connections and refreshes the
gRPC listener. An invalid or partially replaced keypair fails closed.
Do not wait for all node certificates to expire before rotating them.

There is no online CA-rollover protocol in this change. Replacing one
node's CA independently partitions trust. CA compromise requires a
coordinated recovery into a fresh trust domain with newly enrolled nodes,
not just new leaf certificates signed by the compromised CA. Membership
removal alone is not certificate revocation.

### Secrets exposed by historical plaintext replication

Enabling TLS does not protect traffic captured before the upgrade.
An observer positioned on the old Raft path could have obtained resolved
Compose secrets, registry credentials, cluster CA private material, and
ACME account/certificate keys from log or snapshot replication. This does
not imply an arbitrary host can sniff switched LAN traffic.

If exposure is suspected, treat those credentials as compromised: rotate
application/deployment and registry secrets at their authorities, replace
the cluster CA and node identities through a coordinated trusted recovery,
and follow the certificate provider's procedure for ACME account-key
rollover and certificate reissuance/revocation. Restoring an old backup
alone preserves its old trust and secrets. Inventory and protect retained
backups, snapshots, and captures as well; transport TLS does not encrypt
stored Raft data or backups.

## See also

- [`jaco self-upgrade`](../cli/self-upgrade.md)
- [Release and packaging](../contributing/release-and-packaging.md)
- [Recovery](recovery.md)
- [Cluster lifecycle](../concepts/cluster-lifecycle.md)
