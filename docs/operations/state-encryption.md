---
sources:
  - internal/controlplane/seal/
  - internal/controlplane/migration/
  - internal/controlplane/backup/
  - internal/controlplane/raft/node.go
  - internal/ingress/storage/
  - cmd/jaco/state.go
  - cmd/jaco/restore.go
---

# State encryption, migration and recovery

**Before starting or upgrading a daemon, provision its external cluster
keyring. There is no default key, automatic distribution, or plaintext
fallback. Existing plaintext clusters require a coordinated offline
fresh-copy migration, not a rolling upgrade.**

This protects resolved Compose/Jaco manifests, registry passwords, the
cluster CA signing key, ACME account/certificate blobs, and other replicated
application payloads. Authorized runtime consumers still receive the original
values in memory. It is not a named-secret substitution feature: interpolation
and `env_file` resolution continue to happen before Apply.

## Protection and trust boundaries

| Surface | Protection |
|---|---|
| Application commands | AES-256-GCM envelope before entering Raft; logical RPC authorization happens first |
| Bolt log records | An additional envelope authenticates the assigned index, term, type, data and extensions |
| File snapshots | Encrypted portable payload plus an envelope binding snapshot ID, index, term and configuration |
| Backups | Schema 2 encrypts the snapshot and authenticates the exact `meta.json`; no wrapping keys are included |
| ACME fallback cache | Encrypted blobs bound to their hashed cache filenames; no plaintext temporary writes |
| Status/Watch | Raw deployment manifests omitted, including both sides of watch updates |
| Explicit manifest export | `jaco get deployment NAME --show-secrets`, operator-authenticated and scoped to one deployment |

Every envelope uses a fresh random 256-bit data key; the configured active
256-bit wrapping key encrypts that data key. Key IDs and format versions are
public, authenticated identifiers, not secrets. Membership/election metadata
outside encrypted records, filenames, lengths and timing are not hidden.
Raft transport TLS and RPC authorization remain separate required boundaries;
payload encryption is not a substitute for either.

**Every live member remains trusted with all cluster secrets and the CA
signing authority.** Every member holds the complete keyring and decrypted
FSM, including the CA private key. This does not protect against a compromised
live member, root, memory dumps, or a stolen keyring plus matching artifacts.
It does not introduce per-workload/node secret isolation, CA revocation, a
KMS, or protection against rollback of an entire previously valid state.
Use distinct keyrings for distinct clusters.

Node TLS identity files and `wg/private.key` remain local, permission-protected
identity material; they are not the replicated CA key and are not included
in application-state backups. Migration preserves valid local identities.
Docker's runtime metadata, container environments/volumes, operator source
files, application logs, swap and crash dumps are outside this envelope
format. Protect those separately. A plaintext external key file on the same
stolen host does not protect against whole-host theft; use appropriately
isolated credential delivery and encrypted storage/swap for that threat.

## Provision one keyring for the cluster

On a trusted administrative host, create **one** new keyring for a new
cluster. These commands print the filename, never key material:

```sh
sudo install -d -m 0700 /etc/jaco/keys
sudo jaco state keygen \
  --file /etc/jaco/keys/cluster-v1.json \
  --key-id cluster-v1 --data-dir /var/lib/jaco
```

Generation is exclusive: it refuses to overwrite a file or reuse a retained
key ID. The JSON has `version: 1`, an `active` ID, and a `keys` map of IDs to
base64-encoded 32-byte keys. Use the generator rather than passwords or
handwritten examples. The key file must be a regular non-symlink file with
owner-only permissions, outside the resolved data directory. Symlinked
ancestors cannot bypass that separation.

Independently provision the **same complete keyring, active ID and retained
versions** on every member through your trusted credential-management
process. Do not generate a different key on each node. Enrollment exchanges
context-bound proofs of possession, not keys, and rejects mismatches before
an upgraded leader changes membership. The joiner authenticates the response
before persisting identity/state. Mixed-version enrollment is unsupported.

Keep a separately controlled recovery copy. Do not put it in `data_dir`, a
snapshot, the archive it protects, source control, command arguments, or a
backup bundle. Do not colocate it with exported backups under the same
unprotected storage/access policy.

### Packaged non-root systemd service

The packaged daemon runs as `jaco`. A root-owned `0600` file is not directly
readable by that service. Prefer a service-manager credential. After
independent provisioning on each host, install this administrator drop-in
at `/etc/systemd/system/jaco.service.d/state-keys.conf`:

```ini
[Service]
LoadCredential=jaco-state-keys:/etc/jaco/keys/cluster-v1.json
```

Keep the source file root-owned and owner-only; systemd delivers a
service-readable credential outside application state. Leave
`state_key_file: ""` in `jacod.yaml`. If your platform supports
`LoadCredentialEncrypted`, an encrypted credential with the same
`jaco-state-keys` name is also supported: JACO reads the delivered JSON, not
the encrypted source credential.

Direct file provisioning is also supported when the file is owned/readable
only by the daemon's effective user and lies outside `data_dir`:

```yaml
state_key_file: /secure/jaco/cluster-v1.json
```

Resolution order is `state_key_file`, `JACO_STATE_KEY_FILE`, then
`$CREDENTIALS_DIRECTORY/jaco-state-keys`. Environment variables carry paths,
not key values. No interactive unlock is attempted. Unattended restart works
only while the external credential remains available and matches the
authenticated on-disk keyring identity. POSIX owner-only file semantics are
required; filesystems that cannot enforce them are rejected.

## Migrate a legacy plaintext cluster

Plan an approved maintenance window. Stop **all** members and stop writes.
Inventory every node's data directory and all historical backups, disk/VM
snapshots, copied caches, diagnostic exports and storage replicas. Protect
this inventory as sensitive data. Merely enabling encryption for future
writes does not remove secrets in old records or Bolt free pages.

For each stopped member, after provisioning the external keyring:

```sh
sudo jaco state migrate \
  --source-dir /var/lib/jaco \
  --target-dir /var/lib/jaco-encrypted \
  --key-file /etc/jaco/keys/cluster-v1.json \
  --allow-legacy-plaintext
```

The destination must not exist and must not overlap the source. The command
holds the source database read-only against writers and creates a new
database, not a copied/rewritten Bolt file. It preserves logical log indices,
terms, membership/election state, every complete physical snapshot
(including older retained history), and recognized cache artifacts. It
encrypts before writing destination bytes. Old free-page contents are never
copied. Known local identity/metadata files are preserved without changing
their identity format.

All source artifacts remain untouched. Unknown files, standalone CA private
keys, symlinks, incomplete snapshots, unsupported metadata and corruption
stop the operation rather than being skipped or deleted. Separately inspect
and retain/quarantine such artifacts; migrate backups using the next section.
A failed copy retains `state-copy.incomplete` in its new destination. Never
remove that guard to force a startup; keep the source and retry with another
fresh destination after resolving the cause.

The command authenticates its output. Also perform an isolated recovery
drill and verify the desired deployments, registry pulls and certificate
state before switching production. Give the completed destination the
service user's ownership. Either update `data_dir` **and** the service's
`ReadWritePaths`, or, with all daemons still stopped, move the original
directory to protected quarantine and move the completed directory to the
original configured path. These are explicit operator actions, not performed
by the migration command. Start only compatible binaries with identical
keyrings after every member has been prepared.

Do not run an old binary against encrypted state or mix old and new members.
Rollback, if needed, requires the retained original directories and matching
old binaries/credentials in a coordinated isolated recovery; it does not
mean decrypting new state in place. Never run duplicate identities from old
and new directories simultaneously.

## Convert historical backups

Schema-1 backups have no authenticity protection. Accept them only with
trusted provenance and explicit opt-in:

```sh
sudo jaco state reencrypt-backup \
  --input /secure-history/legacy.tar.gz \
  --file /backups/cluster-v1.tar.gz \
  --key-file /etc/jaco/keys/cluster-v1.json \
  --allow-legacy-plaintext
```

The source is unchanged and the output must be new. Schema-2 restore
authenticates metadata and content before creating a destination:

```sh
sudo jaco restore --input /backups/cluster-v1.tar.gz \
  --name node-1 --data-dir /var/lib/jaco-restored \
  --key-file /etc/jaco/keys/cluster-v1.json
```

Use a fresh, isolated drill location and compatible configuration. Restore
never accepts schema 1 directly. An unavailable/wrong key, tampered metadata
or ciphertext is an error, not a request to try plaintext or older state.
After recovery, configure the daemon to use the same keyring and completed
data directory. Lost wrapping keys make their encrypted artifacts
unrecoverable; operator tokens and the public CA cannot replace them.

## Rotate wrapping keys

Rotation is coordinated and offline. On-disk provenance and join proofs bind
the complete ring; changing only `active` or silently dropping/adding a key
is not a supported restart procedure.

Create a new key file with a new ID. `keygen --retain-file OLD` can retain
historical versions if desired, but does not migrate anything. Keep old
recovery keys in separate protected custody until every required old backup
has been converted and restore-tested.

With all members stopped, use the old complete ring to read and the new
complete ring to write fresh copies on every member:

```sh
sudo jaco state migrate \
  --source-dir /var/lib/jaco --target-dir /var/lib/jaco-rotated \
  --key-file /etc/jaco/keys/cluster-v1.json \
  --new-key-file /etc/jaco/keys/cluster-v2.json

sudo jaco state reencrypt-backup \
  --input /backups/cluster-v1.tar.gz --file /backups/cluster-v2.tar.gz \
  --key-file /etc/jaco/keys/cluster-v1.json \
  --new-key-file /etc/jaco/keys/cluster-v2.json
```

These re-encrypt both payloads and outer envelopes, so a new copy need not
depend on an old wrapping key. Update each service credential only when its
matching completed directory is ready. Retire old keys from live keyrings
only through another coordinated fresh copy if the complete ring changes.
Rotate well before AES-GCM's limit of `2^32` encryptions per wrapping key;
count envelopes across all replicas, snapshots, migrations and backups,
not just operator Apply calls.

## Retention and incident response

Encryption cannot revoke copies already exposed. Retain original artifacts
under restricted, encrypted quarantine until recovery is verified and
retention/legal requirements permit disposal. Then use the storage platform's
approved sanitization or cryptographic-erasure process for old volumes,
free pages, filesystem/VM snapshots, replicated storage and backup versions.
Deleting files or rewriting Bolt values is not secure erasure, especially on
SSDs and copy-on-write filesystems. No JACO migration silently destroys that
history or retires a recovery key.

Separately rotate previously exposed deployment passwords, registry
credentials and ACME keys/accounts. Exposure of the replicated CA key may
require replacing the cluster trust root and reenrolling nodes under an
approved recovery plan. Wrapping-key rotation and removing a member do not
revoke a stolen CA signing key.

## Routine views and explicit export

Status and all deployment Watch events omit stored Jaco/Compose YAML. The
runtime, scheduler and ingress continue using the full local FSM.
`jaco get deployment NAME` reports `spec_redacted: true`; it also strips
manifests returned by an older daemon. Update clients that previously
treated Status/Watch as a secret-bearing configuration export.

Use `jaco get deployment NAME --show-secrets` only when an authorized full
export is needed. The RPC requires a valid operator-token or trusted local
socket identity, a deployment filter and no service filter. Output may
contain resolved credentials; protect its destination and terminal history.
There is no unredacted Watch mode.
