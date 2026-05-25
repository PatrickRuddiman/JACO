# Recovery

What to do when the cluster, a node, or a subsystem is unhealthy.
Read top to bottom; the situations are roughly in order of severity
(least to most).

## Single node down (cluster still has majority)

Symptoms: `jaco node list` shows the host with a stale `READY` then
silence; `jaco cluster status` from another node reports `Nodes (N):`
with the down host's `STATUS` no longer transitioning.

What's actually happening: raft sees the peer as unreachable; the
other voters maintain quorum. Existing replicas on the down node are
unreachable but the cluster does not pre-emptively reschedule (no
healthcheck source).

Action:

1. Triage the host: `systemctl status jacod`, `journalctl -u jaco -p
   err -n 200`, dmesg.
2. If recoverable, fix and let the daemon rejoin — no operator action
   needed against the cluster, raft catches the node up from the
   snapshot + log.
3. If unrecoverable, run `jaco node remove --server $LEADER <host>` to
   drain replicas onto surviving nodes. See
   [Cluster lifecycle → graceful remove](../concepts/cluster-lifecycle.md).

## Leader transition

Symptoms: write RPCs return `Error{code: no_leader}` for up to ~10 s;
read RPCs continue to work from any node's local watch cache.

What's happening: the previous leader became unreachable and the
remaining voters are electing a new one.

Action: nothing. Retry the write after a few seconds. `jaco cluster
status` reports the new leader once the election completes. If the
window exceeds 10 s, suspect a network partition (next section).

## Network partition

Symptoms (minority side): `Error{code: quorum_lost}` on writes;
existing replicas keep running; ingress on the minority side keeps
serving routes whose upstreams are local.

Symptoms (majority side): healthy. The partitioned nodes appear
unreachable in `jaco node list`.

Action:

1. Repair the network. The minority side rejoins as followers
   automatically once connectivity returns.
2. Do NOT force a single-node restore on the minority side. That
   creates a split-brain (two clusters with the same id) that JACO
   has no automated reconciliation for.
3. If you must operate the minority side as the new cluster (e.g.
   the majority is permanently lost), follow
   [Total cluster loss](#total-cluster-loss) below.

## Total cluster loss

Symptoms: every node is gone (hardware loss, region outage). You
have a backup.

Action:

1. Provision a fresh host, install JACO at a version compatible with
   the backup.
2. `sudo systemctl stop jaco` if the daemon auto-started.
3. `sudo jaco restore --input <backup>.tar.gz --name $(hostname)`.
4. `sudo systemctl start jaco`.
5. `jaco cluster status` should show the restored cluster id, a
   single voter, and the deployments from the backup.
6. Provision and join the remaining nodes via
   `jaco node issue-join-token` + `jaco node join`.
7. Wait for `jaco node list` to report every node `READY`. Verify
   deployments converge: `jaco status -w`.

See [Backups](backups.md) for the full export → restore workflow.

## Pinned replica is `pending`

Symptoms: `jaco status <dep>/<svc>` reports
`pending: cannot satisfy host placement: <host> unreachable`. No
containers come up.

What's happening: `placement: hosts` requires specific hosts; one or
more are unreachable; the scheduler does not relocate pinned replicas
elsewhere.

Action: either repair the host so it returns to `READY`, or edit the
jaco.yaml to point at different hosts and re-apply. Removing the
pinned host via `jaco node remove --force` is the explicit "stop
trying to place this pinned replica" escape; the deployment goes
`pending: cannot satisfy host placement: <host> removed`.

## Replica stuck in `failed`

Symptoms: `jaco status <dep>/<svc>` shows a replica in `failed` state
with `code: image_pull_failed | docker_error | restart_exhausted`.

What's happening:

- `image_pull_failed` — the runtime retried with exponential backoff
  capped at 1 h; the registry is unreachable, auth failed, or the tag
  doesn't exist. The replica retries on every backoff window without
  resetting attempt count.
- `docker_error` — the docker daemon refused (disk full, daemon
  stopped, kernel issue).
- `restart_exhausted` — the scheduler stopped restarting after 3
  consecutive failures.

Action:

1. Fix the underlying cause (registry, disk, docker daemon).
2. `jaco apply` the same manifest — the apply increments the
   deployment revision, which resets the replica's attempt counter.
   In the `restart_exhausted` case this is the only way to retry.

## Node in `isolation_unavailable`

Symptoms: `jaco node list` reports
`<host>  …  NODE_STATUS_ISOLATION_UNAVAILABLE`. No containers
schedule on the host; other nodes skip it for placement; ingress on
the host still works for routes whose upstreams are remote.

What's happening: the nftables ruleset failed to load (no kernel
support, missing `nft` binary, missing `CAP_NET_ADMIN`) or the
self-test failed.

Action:

1. On the host: confirm nftables is installed (`nft --version`), the
   kernel supports it (`grep -i nf_tables /boot/config-$(uname -r)`),
   and the daemon has the right capabilities (the systemd unit ships
   `AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
   CAP_NET_RAW`).
2. `journalctl -u jaco -p err` for the specific failure.
3. `sudo systemctl restart jacod` once the fix is in. Self-test runs
   again; on success the node transitions to `READY`.

See [Isolation](../concepts/isolation.md).

## Out-of-band edits to the nftables `jaco` table

Symptoms: an `AuditEvent{type: isolation_ruleset_reconciled}` shows up
unexpectedly in `jaco audit`.

What's happening: someone or something modified `inet jaco` out of
band. The 30 s reconcile loop detected drift and atomically restored
the expected ruleset.

Action: investigate **why** something edited the table. JACO will keep
correcting it, but the drift suggests a misbehaving config-management
tool, a stray operator edit, or a security event. The audit event
payload includes a diff summary in `details`.

## Quorum loss after multiple node failures

Symptoms: `jaco apply` from anywhere returns `quorum_lost`. Fewer than
`⌊N/2⌋ + 1` voters are alive.

Action:

1. Recover any of the lost voters if you can — bring them back up and
   they rejoin automatically. Even one returning voter restores
   majority for a 3-node cluster.
2. If recovery is not possible, treat it as
   [Total cluster loss](#total-cluster-loss) and restore from a
   backup.
3. Do not attempt to manually edit raft state on a surviving voter.
   That route ends in corruption.

## See also

- [Backups](backups.md)
- [Troubleshooting](troubleshooting.md)
- [Cluster lifecycle](../concepts/cluster-lifecycle.md)
- [Isolation](../concepts/isolation.md)
