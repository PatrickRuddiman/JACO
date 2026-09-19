---
sources:
  - cmd/jaco/self_upgrade.go
  - internal/packaging/
  - .github/workflows/release.yml
---

# Upgrades

JACO is upgraded **one node at a time** with `jaco self-upgrade`. The
command verifies the release tarball (minisign signature over
`SHA256SUMS`, plus SHA-256 of the tarball), atomically swaps both
binaries, restarts the daemon under systemd, and rolls back on health
failure.

CLI reference: [`jaco self-upgrade`](../cli/self-upgrade.md).

## Why one node at a time

JACO's design commits to a **single static binary per node** plus a
strict version-skew bound: an N+1 CLI must accept commands against an
N daemon within the same major version. gRPC field additions are
backward-compatible; raft FSM apply is binary-compatible across
adjacent versions. So a rolling upgrade — one node up, wait for
raft rejoin, move to the next — is the canonical path.

Cluster-wide coordinated upgrade (`jaco cluster upgrade --all`) is
explicitly **not** in v1; the operator drives the rotation.

## Peer TLS and enrollment compatibility

Before upgrading to mandatory peer CA/SAN verification, check every
existing node's credentials. Older certificates without an advertised
DNS/IP SAN now fail instead of silently connecting; protobuf field
compatibility does not make legacy enrollment tokens or certificates
valid under the new checks.

`OpenRaft` also rejects missing/invalid node CA/keypair material, a CN
identity mismatch, or missing advertised SANs before Raft startup; it
does not fall back to the bootstrap certificate. For restored nodes,
follow the [covered-IP recovery guidance](recovery.md#restored-node-credentials-and-addresses)
rather than assuming automatic certificate reissuance.

1. Back up cluster state and preserve node credentials. Inventory each
   daemon's OS/configured hostname, advertised Raft and gRPC host
   components, and any additional IP/DNS names operators dial.
2. Independently verify the cluster CA bundle's provenance through an
   authenticated existing member (local socket, already CA-verifying
   operator TLS, verified SSH, or trusted configuration management).
   Ensure each daemon can read its `$JACO_DATA_DIR/node/ca.crt`.
3. Verify each existing leaf's chain, dates, identity, required SANs,
   and server/client-auth EKUs. For example, substituting the actual
   hostname and every dial IP/DNS name:

   ```sh
   sudo openssl x509 -in /var/lib/jaco/node/<hostname>.crt \
     -noout -subject -dates -ext subjectAltName,extendedKeyUsage
   sudo openssl verify -CAfile /path/to/cluster-ca.crt \
     -purpose sslserver -verify_ip 10.0.0.6 /var/lib/jaco/node/<hostname>.crt
   sudo openssl verify -CAfile /path/to/cluster-ca.crt \
     -purpose sslserver -verify_hostname node-2.internal /var/lib/jaco/node/<hostname>.crt
   sudo openssl verify -CAfile /path/to/cluster-ca.crt \
     -purpose sslclient /var/lib/jaco/node/<hostname>.crt
   ```

4. If a legacy certificate is unsuitable, arrange approved
   re-enrollment or independently provision a correctly signed
   replacement matching the local private key, intended identity, all
   required SANs, and both EKUs. Preserve quorum and follow the existing
   certificate activation/restart process. **Do not delete live Raft
   state or node keys to clear a TLS error.**
5. Update token-only enrollment automation before upgrading:
   `jaco node issue-join-token --node-name <hostname>` now requires a
   separately scoped token per node, with repeatable `--san <DNS-or-IP>`
   approvals for every advertised host beyond the implicit hostname
   and any extra dial aliases. Unknown/legacy unscoped tokens must be
   reissued. Coordinate issuer and joiner upgrades; an old issuer
   cannot supply the required scope. Provision the authentic CA file
   before `jaco node join --peer <member>:7000 --token <single-use>
   --ca-cert <trusted-ca.pem>` (or use `JACO_CA_CERT` / the existing
   default CA file). No missing-CA or self-signed bootstrap fallback
   exists.

New bootstrap certificates include explicit advertised DNS aliases and
local/private IPs. New join certificates include only token-approved
SANs; CSR aliases do not grant additional names.

Trusted CA-signed leaf renewals and key rotations need no leaf-pin
updates. Existing dynamicTLS/restart swaps remain unchanged. New peer
dials reload `node/ca.crt`, so planned CA changes require explicitly
provisioning overlapping old/new bundles out of band before swapping
certificates. There is no automatic CA acceptance from remote RPCs, no
new renewal RPC, and no automatic CA-rotation workflow. See
[Auth and tokens](../concepts/auth-and-tokens.md#peer-grpc-verification-and-certificate-changes).

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

## See also

- [`jaco self-upgrade`](../cli/self-upgrade.md)
- [Release and packaging](../contributing/release-and-packaging.md)
- [Recovery](recovery.md)
- [Cluster lifecycle](../concepts/cluster-lifecycle.md)
