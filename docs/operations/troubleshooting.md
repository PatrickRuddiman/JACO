---
sources:
  - internal/controlplane/grpc/
  - internal/runtime/compose/
  - internal/runtime/pull/
  - internal/discovery/firewall/
  - internal/ingress/
  - internal/discovery/dns/
  - proto/jaco/v1/entities.proto
---

# Troubleshooting

Error codes you will actually see, what they mean, and how to clear
them. The closed code set lives in
[Status and errors](../concepts/status-and-errors.md); this page is
the action-oriented index.

For incident-shaped problems (a node is down, the cluster lost
quorum, you need to restore from backup), jump straight to
[Recovery](recovery.md).

## `cluster_uninitialized`

```
Error: cluster_uninitialized: …
```

The daemon is up but no cluster has been bootstrapped or joined yet.
Every RPC except `Cluster.{Init, Join, Status}` returns this.

Clear it by either initializing a new cluster on this host or joining
an existing one:

```sh
sudo jaco cluster init                                   # new cluster
# or
sudo jaco node join --peer <leader>:7000 --token <hex>   # join existing
```

## `cluster_already_initialized`

`jaco cluster init` against a host whose `$JACO_DATA_DIR/raft/` is
already populated. Either the host is already a cluster member (check
`jaco cluster status`), or there's stale raft state from a prior
install.

If you actually intend to wipe and start over: `sudo systemctl stop
jaco && sudo rm -rf /var/lib/jaco/* && sudo systemctl start jaco`, then
`sudo jaco cluster init`. **This destroys all cluster state on the host.**

## `no_leader`

A write RPC arrived while raft is mid-election. Retry after a few
seconds. If the condition persists past ~10 s, suspect a network
partition — see [Recovery → Network partition](recovery.md#network-partition).

## `quorum_lost`

The local side of a network partition does not have a majority.
Writes are rejected; reads continue. See [Recovery → Network
partition](recovery.md#network-partition).

## `token_invalid` / `token_revoked`

Cross-host RPC with a bearer token the cluster doesn't recognize, or
recognizes but has revoked.

- `token_invalid` — typo in `JACO_TOKEN`, wrong cluster, or never-issued.
- `token_revoked` — the token was explicitly revoked via
  `jaco token revoke`.

Mint a fresh one if you legitimately need access:

```sh
# from a host that already has a working operator token
jaco token issue --server $LEADER --name <your-identity>
```

`bootstrap` is the only identity that exists out of the box. If you
revoke it without minting a replacement first, you lock yourself out
of the cluster (the unix-socket path on cluster nodes is the only
remaining way in).

## `local socket /var/run/jaco/jaco.sock is not available`

```
no --server given and local socket /var/run/jaco/jaco.sock is not available:
pass --server (host:port) with --token, or run this command on a cluster node
with the jaco daemon socket
```

A `jaco`-group member on a cluster node runs an on-node command (no
`--server`) and the CLI can't find the control socket — even though the
daemon is healthy and `ss -xlp | grep jaco` shows it listening on exactly
that path.

The classic signature is the socket being visible to `jacod` but not to the
host: `ss` proves it's listening, yet `ls -lL /var/run/jaco/jaco.sock`
reports "No such file or directory" for root and group members alike. That
means `jacod` bound the socket inside a **private mount namespace** — the
path resolves only inside the daemon's own mount view. Confirm with:

```sh
nsenter -t "$(pgrep -x jacod)" -m ls -lL /run/jaco/   # socket present in-ns?
ls -lL /run/jaco/                                      # absent on the host?
```

If the socket shows up under `nsenter` but not on the host, `jacod` bound
the socket itself inside its private mount namespace instead of inheriting
it from systemd (issue #167). The fix is **systemd socket activation**: the
shipped `jaco.socket` unit makes PID 1 create and bind `/run/jaco/jaco.sock`
in the host mount namespace and pass the daemon the file descriptor, so the
socket is host-visible no matter how the daemon's filesystem is sandboxed.
`jaco.service` pulls it in via `Requires=jaco.socket` / `After=jaco.socket`,
and `jacod` logs `using systemd-activated local-control socket` on startup.

If `jaco.socket` is missing or disabled, install it and reload:

```sh
sudo install -m 0644 jaco.socket /lib/systemd/system/jaco.socket
sudo systemctl daemon-reload
sudo systemctl restart jaco        # Requires= starts jaco.socket first
ls -lL /run/jaco/jaco.sock         # now present on the host
```

The socket stays mode `0660`, group `jaco` (set by the `jaco.socket` unit),
so any `jaco`-group member can drive the daemon over it with no token.
Relatedly, keep the symlinked `/var/run/jaco` and bare `/run` out of the
service's `ReadWritePaths` — listing them adds a second, namespace-private
bind mount that would re-shadow the host-visible `/run/jaco`.

## `validation_failed`

The manifest schema or cross-check failed at apply. Details include
the offending field, service, or network. Common shapes:

- jaco.yaml uses an unknown top-level key — only `deployment`,
  `services`, `routes` are accepted.
- A service block uses an unknown key — see
  [`jaco.yaml`](../manifests/jaco-yaml.md) for the closed set.
- `placement: hosts` without a `hosts:` list.
- A jaco service has no matching compose service.

Lint locally first:

```sh
jaco validate --jaco ./hello/jaco.yaml --compose ./hello/docker-compose.yml
```

## `unknown_service` / `unknown_host` / `unknown_network`

Apply was rejected because:

- `unknown_service` — `services[*].name` doesn't match any compose key.
- `unknown_host` — `services[*].hosts[*]` is not a known cluster member.
  Check `jaco node list`.
- `unknown_network` — a compose service references a network not
  declared in the top-level `networks:` block. Either add the network
  declaration or remove the reference.

## `reserved_port`

A compose service publishes a host port in `{80, 443}`. Those belong
to Caddy's HTTP/S ingress. Move the service onto a non-reserved port
and declare a `routes:` entry in jaco.yaml to expose it publicly via
Caddy, **or** publish on a different host port and let JACO's L4
router forward.

## `legacy_compose_field`

The compose file uses a v1/v2 spelling that the modern compose spec
dropped. JACO's loader detects compose-go's "additional properties"
rejection and re-emits a typed error that names the modern
equivalent. The complete legacy → modern map:

| legacy key | modern equivalent |
|---|---|
| `log_driver` | `logging.driver` |
| `log_opt` | `logging.options` |
| `net` | `network_mode` |
| `volume_driver` | no direct equivalent; use long-form `volumes:` with `driver:` |
| `dockerfile` (top-level service key) | `build.dockerfile` |

`details.field` names the offending key; `details.modern_equivalent`
repeats the table value. See
[Migration → Legacy compose spellings](migration.md#legacy-v1v2-spellings)
for porting guidance.

## `env_file_unresolved`

The daemon received a compose document that still carries `env_file:`.
The CLI is supposed to fold every referenced file into `environment:`
client-side before sending; this error means an old CLI is talking to
a new daemon, or the document was synthesized by a tool that bypassed
the CLI's resolver. Upgrade the CLI (`jaco self-upgrade`) or run the
resolution step yourself.

## `load environment file <path>: …`

`jaco apply` failed before talking to the daemon: the
[`environment: <path>`](../manifests/jaco-yaml.md#environment) on the
jaco.yaml resolved to a path the CLI could not read. The path is
resolved **relative to the jaco.yaml's directory** (or honored
verbatim if absolute). The most common shape is a missing file —
stage the `.env` next to `jaco.yaml` before re-applying. The error
is CLI-side only; the cluster state is untouched.

## `interpolate <path>: ... required variable <NAME> is missing a value`

The compose document referenced `${NAME:?msg}` (the required-variable
form), and `NAME` is absent from the `environment:` file the CLI
loaded. The full chain reads
`interpolate <compose-path>: interpolate ${VAR} at line N col M: required variable <NAME> is missing a value: <msg>`
— the line/column point at the offending site in the original
compose file. Either:

- add `NAME=…` to the env file the jaco.yaml points at, or
- relax the reference to `${NAME:-<default>}` if the variable is
  genuinely optional.

Process-environment passthrough is NOT honored — only the env file
from jaco.yaml's `environment:` participates in the interpolation
map. CLI-side only; the cluster state is untouched.

## `interpolate <path>: ... invalid template`

The CLI's interpolation step rejected a malformed `${…}` reference
(e.g. an unclosed brace). The full chain reads
`interpolate <compose-path>: interpolate ${VAR} at line N col M: invalid template …`
— the line/column point at the offending site. Fix the reference
and re-apply. CLI-side only.

## `staging self-check passed; promoting to prod` (every 10 s, forever)

Pre-v0.3.3 symptom. The stage-first controller's tick decision was
"domain not in staging AND no prod cert in raft → stage it", which
fired on the same 10 s tick that promoted the staging cert — Caddy's
prod ACME order never had time to complete before the controller
re-staged the domain and flipped the policy back. `journalctl -u
jaco | grep -c 'promoting to prod'` grew without bound; raft state
stayed `staging` forever; browsers kept seeing `(STAGING) …`
issuers. v0.3.3 added a 5-minute `PendingProdWindow` that holds the
domain out of re-stage until Caddy either completes the prod order
or the window expires (issue #154). If you see this on v0.3.2 or
earlier, upgrade to v0.3.3+ via `jaco self-upgrade`.

## `staging` cert still served after promote (v0.3.3 / v0.3.4)

The promote log fired exactly once but the browser still sees a
staging cert. Pre-v0.3.5 there were two reasons the prod ACME order
never actually fired:

- **v0.3.3 and earlier** — promote only flipped the automation
  policy's CA URL; the staging cert blob in raft and on-disk + the
  in-process certmagic cache all remained valid, and certmagic's
  maintainer treats valid-for-90-days leaves as "do not re-obtain".
  Workaround: `sudo systemctl stop jaco && sudo rm -rf
  /var/lib/jaco/ingress/cache /var/lib/jaco/.config/caddy/autosave.json
  && sudo systemctl start jaco`.
- **v0.3.4** — promote wipes the raft + on-disk blobs (issue #158,
  PR #162) so a daemon restart now lands a prod cert without manual
  rm. But certmagic's in-process cache still held the staging leaf,
  so without restart the served cert stayed staging. Workaround:
  `sudo systemctl restart jaco` after the promote log fires.

v0.3.5 closes both gaps: the promote path also calls
`cachepoke.EvictManaged(domain)` which drops the cached leaf from
caddy's `caddytls.certCache` via `go:linkname`, so the next
handshake after promote misses cache, misses storage under the
prod-CA key, and triggers obtain (issue #163). End-to-end on a
fresh cluster the cert flips from `(STAGING) …` to a real LE prod
intermediate within seconds of the first post-promote handshake.
No workaround needed; `jaco self-upgrade` to v0.3.5+ and let the
next stagefirst tick run.

## `cert audit emit skipped: cachepoke: caddytls cert cache not yet provisioned`

v0.3.5+ log line. `cachepoke.EvictManaged` ran before Caddy's TLS
app finished provisioning its in-process cert cache (the
`go:linkname`'d singleton is still nil). In practice this means a
promote fired before the first rebuild completed — extremely
unlikely outside a stagefirst race during cold start. The log line
is a warning, not an error; the storage wipe still happened and a
daemon restart will land the prod cert. If you see it persistently,
file an issue with the full journal around the promote event.

## Replicas stuck in `pending` despite the container being healthy (pre-v0.3.2)

A `tls: auto` route's domain comes up but the replicas behind it
sit in `pending` indefinitely; `docker inspect` shows the container
with `State.Status: running` and a passing image-built-in
`State.Health.Status: healthy`. `jaco status` keeps reporting
`pending` for those replicas, and any service with a `depends_on`
reference to them gets deferred forever.

Pre-v0.3.2 the health watcher's per-replica `consecutiveRunning`
counter was reset on every reconciler re-dispatch (and the
reconciler re-dispatches on every safety tick, every
`ReplicaDesired` event, every sibling state change). The counter
never reached `HealthyConsecutiveCount = 5`, so the fallback path
for healthcheck-less containers never fired. v0.3.2 made the
`Watcher.Start` call idempotent for the same
`(replica_id, container_id)` pair, so the counter accumulates
across re-dispatches and reaches 5 in ~5 seconds (issue #152).

`healthcheck: { disable: true }` was also affected because pre-v0.3.2
the daemon's projection layer treated `disable` as "registered
healthcheck" and waited for a `State.Health.Status` Docker would
never produce. v0.3.2 returns nil from
`healthcheckFromCompose` when `Disable=true`, so the fallback path
owns these the same as truly healthcheck-less services.

If you see this on v0.3.1 or earlier, `jaco self-upgrade` to v0.3.2+.

## Container reused with stale env values after `.env` rotation (pre-v0.3.1)

Operator rotates a value in their `.env` (top-level
`environment: .env` in `jaco.yaml`), re-applies, gets `Applied
revision: N+1`, but `docker exec <container> env` still shows the
previous values. Restart, no change. The replica is pinned to the
container created from revision `N`.

Pre-v0.3.1 the scheduler's upsert gate compared only `(Host,
Image)`. Any other compose change — env values, healthcheck
command, mounts, labels — yielded `continue` → no
`ReplicaDesiredUpsert` → `RaftIndex` never bumped → `lifecycle.Start`'s
`matchesRaftIndex` returned true → container kept as-is with the
stale env baked in at create time (container env is immutable for
the life of a container). The only escape was `docker rm -f` per
stuck replica.

v0.3.1 added `ReplicaDesired.spec_hash`: a SHA-256 of the canonical
per-service slice of the resolved compose YAML. The upsert gate now
includes the hash, so env-value rotation flips it, the upsert
fires, `RaftIndex` bumps, and the runtime reconciler recreates the
container with the new env (issue #148). If you see stuck env on
v0.3.0, `jaco self-upgrade` to v0.3.1+ and re-apply once; the
next reconcile recreates every replica with the current resolved
spec.

## `output format "<fmt>" not implemented yet; only "table" is supported (#156)`

v0.3.4+ CLI explicitly rejects `-o json` / `-o yaml` on every
subcommand except `jaco audit` (the only one that actually
implements non-table output). Pre-v0.3.4 the flag was silently
ignored — CI pipelines piping `jaco status -o json | jq .` got a
`parse error` from jq because the actual output was the table
format. The hard rejection makes the breakage visible. Use `jaco
audit -o json` for any structured-output need; for `status` /
`logs` / etc., either parse the table or watch for v0.4.0 which
may extend `-o json` coverage.

## `port_conflict`

Apply rejected because two compose services in the deployment publish
the same host port, or because a host port the deployment wants is
already published by another deployment cluster-wide. The proto's
`TCPRoute` is keyed by `published_port` cluster-wide: there is no
per-host scope.

Pick a different host port on the conflicting `ports:` entry, or
`jaco delete` the deployment that already owns the port. Reserved
ports `80` and `443` continue to surface as `reserved_port`, not
`port_conflict`.

## `PermissionDenied` (privileged admission)

Apply rejected because a compose service set `privileged: true` or
a non-empty `security_opt:` list and the calling operator's token
lacks `allows_privileged=true`. The first offending service is named
in the message. Two fixes, depending on intent:

- The workload genuinely needs the privileged bit — mint a privileged
  operator token (`jaco token issue --server $LEADER --name <id>
  --allow-privileged`) and re-apply with that token.
- The workload should not be privileged — drop `privileged:` /
  `security_opt:` from the compose file and re-apply with the
  existing token.

If the rejection is the schema-time half instead (the service is
missing `labels: { "jaco.io/allow-privileged": "true" }`), the error
code is `validation_failed`, not `PermissionDenied` — see
[`jaco validate`](../cli/validate.md) and
[Supported compose fields → Privileged services](../manifests/compose.md#privileged-services).

## `deployment_not_found` / `no_previous_revision`

From `jaco rollback`:

- `deployment_not_found` — the named deployment is not in raft
  state. Confirm spelling with `jaco status --server $LEADER`.
- `no_previous_revision` — the deployment has only ever been applied
  once, so there's nothing to roll back to. Apply a new revision
  forward instead.

## `replicas_exceed_pinned_hosts`

`placement: hosts` with `replicas` greater than the number of hosts
listed. Either shrink `replicas` or expand `hosts`. JACO does not
co-locate two replicas of the same service on one pinned host.

## `image_pull_failed`

The runtime gave up after the exponential backoff window failed to
acquire the image. Causes: bad image tag, registry unreachable, auth
failure, network egress blocked.

Fix the cause, then `jaco apply` the same manifest — the apply
increments the deployment revision, resetting attempt counts and
states cleanly. Repeating the *same* revision will not unstick a
replica past `restart_exhausted`.

## `cert_failed`

ACME issuance for a `tls: auto` domain failed past the backoff cap (1 h).
Causes:

- DNS for the domain doesn't resolve to a cluster node IP.
- HTTP-01 challenge can't reach a node on port 80 (firewall in front of
  the cluster).
- The public CA is rate-limiting (use `acme_skip_staging: false` to
  burn staging failures cheaply first; see
  [Configuration](../configuration.md)).

While `cert_failed` is in effect, plaintext HTTP for the domain
continues to serve. `jaco audit --type certificate_failed` carries the
last error per attempt.

## `docker_error`

The docker daemon refused or errored. Almost always a host-side issue:
disk full, docker daemon stopped, kernel mismatch.

`journalctl -u jaco -p err -n 200` and `journalctl -u docker -p err
-n 200` on the affected node.

## `isolation_unavailable`

The node could not bring up its nftables ruleset, or the self-test
failed. The node is in raft membership but refuses to host containers.
Other nodes see this in `jaco status` and skip it for placement.

See [Recovery → Node in
`isolation_unavailable`](recovery.md#node-in-isolation_unavailable).

## `subnet_pool_exhausted`

The IPAM pool ran out of `/24`s. Default
[`ipam_pool: 10.244.0.0/16`](../configuration.md) gives 256
allocations.

Mitigations:

1. Delete deployments you no longer need (`jaco delete`) — that frees
   their subnets back into the pool.
2. Bump `ipam_pool` to a larger `/16` and restart every daemon. The
   pool must be a `/16` exactly; pre-existing allocations from the
   smaller pool remain valid as long as they fall inside the new pool.

## `upgrade_verification_failed` / `upgrade_failed`

From `jaco self-upgrade`:

- `upgrade_verification_failed` — minisign signature or SHA-256
  checksum mismatch. The CLI **did not** touch the binaries. Re-verify
  the download URL and the local clock (signature timestamps).
- `upgrade_failed` — the binary swap succeeded but the new daemon
  failed `--version` within 3 s; the CLI rolled both binaries back
  from `.prev` and restarted. Investigate via `journalctl -u jaco`.

See [`jaco self-upgrade`](../cli/self-upgrade.md) and
[Upgrades](upgrades.md).

## Node shows `NONVOTER`

Not an error. `jaco cluster status` rendering a node as `[READY,
NONVOTER]` is the steady state for at least one peer in any cluster
whose member count is even or whose member count exceeds 7. The
leader-only voter-set reconciler holds the cluster at an odd voter
count and caps it at 7 — a 4-member cluster runs with 3 voters and 1
nonvoter; an 8-member cluster runs with 7 voters and 1 nonvoter. See
[Cluster lifecycle → Voter-set policy](../concepts/cluster-lifecycle.md#voter-set-policy)
for the full table.

It is **also** the transient state for every freshly-joined node
during the reconciler's `PromoteAfter` window (3 s by default). If a
nonvoter persists past that window in a cluster whose target voter
count exceeds the current voter count, the reconciler is either not
running or self-gated as a follower — check:

- `jaco cluster status` from each node: only the one whose
  `Leader:` line points at itself runs the reconciler.
- Leader's journal: `journalctl -u jaco | grep "membership reconciler"`
  should show `membership reconciler started` at boot and `promoting
  nonvoter to voter` lines as nodes join.
- Follower suffrage shows as `?` (not `NONVOTER`) — that's normal,
  the CLI refuses to render a suffrage from a follower's stale view
  of raft configuration.

## Unexpected `jaco_<deployment>_<key>` volume names on disk

Not an error. As of v0.2.0, every named volume declared in a compose
file lands on each docker host as `jaco_<deployment>_<key>` (e.g.
`jaco_app_pgdata`) instead of the bare `<key>` it would carry under
`docker compose up`. This stops two unrelated JACO deployments that
happen to declare the same bare key (`data`, `pgdata`, `logs`, …) on
the same host from silently mounting the same backing storage.

- The path inside the container is unchanged — the service still
  reaches its volume at the mount path it declared.
- Bind mounts (`/host/path:/in/container`) and anonymous volumes
  (`/in/container`) are untouched.
- The compose-portable escape hatch is `volumes.<key>.name:
  <literal>` (or `external: true`) at the top level of the compose
  file — JACO uses the literal docker volume name verbatim,
  unprefixed, so the storage can be shared across stacks or pre-seeded
  outside JACO.

See [Migration → How JACO names volumes](migration.md#how-jaco-names-volumes)
for the full mechanics and the migration path for a stack that
previously assumed bare names.

## Spurious follower log lines (silenced)

Two startup-window log patterns from older builds are now silenced —
if you see them in current logs, suspect either a stale binary on
that node or a real raft / network problem rather than a benign
startup race:

- `firewall.Reconciler.Tick failed` paired with
  `Audit(ISOLATION_RULESET_RECONCILED action=applied) failed` — the
  firewall reconciler's audit/status writes used to call
  `node.Apply` directly, failing with `ErrNotLeader` on every
  follower (issue #88, fixed via the `applyOrForwardCommand` shim;
  see [Isolation → Leader-forwarded audit and
  status](../concepts/isolation.md#leader-forwarded-audit-and-status-issue-88-112-113)).
  A freshly-joined follower's first tick still raced ahead of leader
  discovery; the reconciler now gates its first tick on
  `node.Leader() != ""` (issue #113).
- `node is not the leader - storage is probably misconfigured` from
  Caddy's tls maintenance loop, every ~10 minutes on every
  non-leader node — the cert-storage `Lock` write used to fail with
  `ErrNotLeader` on followers (issue #112). It now forwards to the
  leader via the same shim; see
  [Ingress → Custom CertMagic storage](../concepts/ingress.md#custom-certmagic-storage).

## Where to look next

- Daemon logs: `journalctl -u jaco -p info` (or `-p err`).
- Filter by subsystem: `journalctl SUBSYSTEM=scheduler -f`.
- Audit events: `jaco audit --since 1h --server $LEADER`.
- CLI debug logs: `-v` or `--log-level debug`.
- Telemetry: `JACO_OTLP_ENDPOINT` on `jacod` if you have an OTel
  collector in your stack. See
  [Observability](../concepts/observability.md).

## See also

- [Status and errors](../concepts/status-and-errors.md)
- [Recovery](recovery.md)
- [`jaco audit`](../cli/audit.md)
- [Observability](../concepts/observability.md)

## External hostnames fail after exactly 5 s from inside containers (pre-v0.3.6)

`getent ahosts api.github.com` (or any other external name) from
inside any container exits 2 after exactly 5 seconds; internal
service names (`redis`, `web`, `<service>.jaco.internal`) resolve
sub-millisecond. The 5 s is libc's default per-nameserver timeout,
not anything JACO chose.

Pre-v0.3.6 the per-bridge DNS responder forwarded external names
through Go's default `net.LookupHost`, which inside a daemon
process binding multiple bridge gateway IPs had failure modes
(slow `resolv.conf` scan, NSS quirks under CGO, IPv6 fallback to
unreachable nameservers) that consistently exceeded the libc
deadline. The forwarder was wired correctly — the implementation
it called was the wrong tool.

v0.3.6 replaces it with an explicit `miekg/dns` client driven
against an ordered upstream chain
([`internal/discovery/dns/forwarder.go`](../../internal/discovery/dns/forwarder.go))
with a per-upstream deadline (default 2 s) and SERVFAIL semantics
for downstream-resolver retry behavior (issue #165). Upstreams
default to `/etc/resolv.conf` at startup; override with
[`dns.forwarders`](../configuration.md#dns) in `jacod.yaml`.

If you see this on v0.3.5 or earlier, `jaco self-upgrade` to
v0.3.6+ and the next restart of the daemon binds the new
forwarder. Verify with the canonical repro:

```sh
CID=$(sudo docker ps --format '{{.ID}}' --filter 'name=<any-bench>' | head -1)
time sudo docker exec $CID getent ahosts api.github.com
# pre-fix:  exits 2 after ~5.000 s
# post-fix: exits 0 in < 0.1 s
```

## External hostnames SERVFAIL on v0.3.6+ with `dns: no upstream resolvers configured ...` in the journal

v0.3.6+ logs a one-line WARN at daemon start when neither
`dns.forwarders` is set NOR `/etc/resolv.conf` yields a usable
nameserver (every entry was either malformed or filtered as a loop
source — `127.0.0.11`, `10.244.*.1`). The responder then SERVFAILs
every external query rather than NXDOMAIN'ing it (downstream
resolvers retry SERVFAIL, negative-cache NXDOMAIN).

Fix: set `dns.forwarders` explicitly in `jacod.yaml`:

```yaml
dns:
  forwarders:
    - 1.1.1.1
    - 9.9.9.9
```

`sudo systemctl restart jaco` to pick up the change.

## `dns.forwarders[…]: 127.0.0.11 is docker's embedded resolver; configuring it as an upstream would create a forwarding loop`

Operator-supplied `dns.forwarders` entry contained Docker's
embedded resolver address. Containers reach the bridge responder
THROUGH `127.0.0.11`, so configuring it as our upstream would
loop every query forever. Same error shape for any `10.244.*.1`
(JACO bridge gateway). Remove the entry; the daemon parses
`/etc/resolv.conf` at startup and uses every real upstream there
automatically.

