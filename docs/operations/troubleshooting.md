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
