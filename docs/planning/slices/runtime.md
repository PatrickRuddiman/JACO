Parent spec: [spec.md](../spec.md) · Design: [design.md](../design.md)

# JACO — runtime

## §1 Summary

Per-node docker engine driver. Subscribes to `ReplicaDesired` filtered by `host = self` and reconciles container state on the local docker daemon. Owns image pull, container lifecycle, healthcheck observation, named-volume/bind-mount setup, and per-replica log tail serving the cross-node `Logs` RPC. Writes `ReplicaObserved` back through the control-plane.

## §2 Codebase reconnaissance

Greenfield: no existing system to reconcile. Decisions below are unconstrained.

## §3 Decisions

1. **Docker engine SDK.** Options: `github.com/docker/docker/client` (moby), `fsouza/go-dockerclient`, raw HTTP. **Chosen:** moby/moby official client. Rationale: authoritative for the engine API surface; tracks new fields/endpoints first.
2. **Compose parser.** Options: `compose-spec/compose-go`, hand-rolled YAML struct. **Chosen:** `compose-spec/compose-go`. Rationale: reference implementation; full schema coverage for both honored and rejected fields; same library docker compose uses.
3. **Healthcheck strategy.** Options: docker built-in healthcheck poll, JACO-side probe, both. **Chosen:** docker built-in healthcheck via `ContainerInspect.State.Health`. Rationale: honors compose `healthcheck` verbatim per spec; no new config knobs; no in-container probe traffic.
4. **Log tail mechanism.** Options: `ContainerLogs(follow=true)`, `ContainerAttach`, raw file read. **Chosen:** `ContainerLogs(stdout+stderr, follow=true, since=…)`. Rationale: stable API surface; includes historical lines for `--since`; engine handles multiplexing.

## §2.5 Additional decisions

5. **How runtime receives its replica set from the control-plane.** Options: server-side filtering (`Watch.Subscribe(ReplicaDesired, host=self)`), client-side filter on a full stream. **Chosen:** server-side filter. Rationale: keeps watch-stream bandwidth tight on large clusters; control-plane already constructs filtered subscriptions.
6. **Image pull retry policy.** Options: exponential backoff capped at 1h, fixed retries, no retry. **Chosen:** exponential backoff (1s → 2s → 4s → … → 1h cap), no upper attempt count; reset on next `Deploy.Apply`. Rationale: mirrors the spec's cert-issuance retry shape; transient registry outages recover automatically.

## §4 Contracts & shapes

Module layout under `internal/runtime/`:

- `internal/runtime/runtime.go` — `Runtime` struct started by `jaco serve` on every node. Holds the docker client, the watch subscription, and the per-replica goroutine pool.
- `internal/runtime/reconcile.go` — receives `Event<ReplicaDesired>`; dispatches `start | update | stop | remove` per replica id.
- `internal/runtime/compose.go` — wraps compose-go to load and validate the compose file referenced by a deployment; produces a `ContainerSpec` typed for the moby client (image, env, volumes, labels, healthcheck, ulimits, cap_add/drop, sysctls, etc.).
- `internal/runtime/pull.go` — pulls images with backoff; emits `ReplicaObserved{state: pulling}` during pull and `state: failed, code: image_pull_failed` on terminal error.
- `internal/runtime/lifecycle.go` — `start`, `stop`, `remove`, `inspect`; idempotent (start is a no-op if a matching container by label already exists).
- `internal/runtime/health.go` — polls `ContainerInspect` every 1s for replicas not yet `running`, every 5s for replicas in `running`; translates `State.Health.Status` to `ReplicaObserved.state`.
- `internal/runtime/logs.go` — handles `Internal.Logs(replica_id)` peer RPC by opening `ContainerLogs(follow=true, since=...)` and streaming back; demultiplexes stdout/stderr per docker's frame format; tags each line with replica id + stream + host.
- `internal/runtime/volumes.go` — pre-flights named volume creation (`VolumeCreate` if absent) and validates bind-mount source paths exist + are readable.

ContainerSpec → docker CreateContainer mapping (closed set, only fields from spec §3 In):

- compose `image`, `command`, `entrypoint`, `environment`, `env_file`, `working_dir`, `user` → standard fields.
- compose `volumes` → `Mounts` with type=volume or type=bind.
- compose `healthcheck` → `Healthcheck{Test, Interval, Timeout, Retries, StartPeriod}`.
- compose `cap_add`, `cap_drop`, `sysctls`, `ulimits`, `read_only`, `tmpfs` → HostConfig fields.
- compose `labels` → merged with JACO-managed labels (see below).
- compose `restart` → **ignored** (per spec §2 failure mode); JACO scheduler owns restart decisions.
- compose `depends_on` → ordering only; runtime starts replicas in topological order within a deployment apply.
- compose `ports` → **ignored** for ingress purposes (spec: "documentation only"); JACO does not publish container ports to the host; ingress is via Caddy + DNS.
- compose `networks` → honored; runtime attaches the container to one or more `jaco-<deployment>-<network>` bridges (created by discovery slice). Services with no declared networks attach to `jaco-<deployment>-_default`.

JACO-managed container labels (every container set by runtime carries these):

- `jaco.cluster_id`
- `jaco.deployment`
- `jaco.service`
- `jaco.replica_id`
- `jaco.replica_index`
- `jaco.raft_index` (the raft index of the ReplicaDesired that caused creation)

These labels are the runtime's source of truth for "is this container ours and what is it?" — used by reconcile to find orphans on restart.

Replica state transitions written to control-plane (closed set per spec):

- `pending` — ReplicaDesired received, image not yet pulled.
- `pulling` — `pull.go` started; intermediate state.
- `running` — container started, first `State.Health.Status = healthy` observed (or `none` if no healthcheck declared, then `State.Status = running` for 5s suffices).
- `degraded` — `State.Health.Status = unhealthy` observed.
- `updating` — set by scheduler during rolling update; runtime reads but doesn't write this state.
- `failed` — terminal error (image_pull_failed, docker_error, restart_exhausted from scheduler).
- `stopped` — replica removed from desired set; container stopped + removed.

## §5 Sequence

Daemon startup on each node:

1. `jaco serve` creates `Runtime`; opens docker client to `/var/run/docker.sock`.
2. Reconciles existing local containers with JACO labels: for each `jaco.replica_id` found, query control-plane for the ReplicaDesired; if missing or stale, stop+remove (cleanup of orphans from prior crash).
3. Opens `Watch.Subscribe(ReplicaDesired, host=self)`; on catch-up phase, reconciles each desired replica.
4. Starts health poller goroutines per running container.

Replica create:

1. Watch event: `Event<ReplicaDesired>{Added, after: r}` arrives.
2. `reconcile.go` queues a `start(r)` op.
3. `volumes.go` ensures named volumes exist and bind-mount sources are valid.
4. `pull.go` pulls `r.image` if not present locally; emits `ReplicaObserved{state: pulling}` once, then re-emits state changes.
5. `lifecycle.go` creates container with labels, healthcheck, mounts; starts it; emits `ReplicaObserved{state: pending}`.
6. `health.go` polls; on first `healthy` (or `running + no healthcheck declared`), emits `ReplicaObserved{state: running, started_at, container_id, last_health_at}`.

Image pull failure:

1. `pull.go` retries with exponential backoff (1, 2, 4, … capped at 3600s).
2. Each terminal-by-attempt failure emits `ReplicaObserved{state: failed, code: image_pull_failed, message: <last error>, details: {attempt, next_retry_at}}`.
3. On every retry the state moves back to `pulling`; on success transitions normally.
4. A new `Deploy.Apply` for the deployment resets attempt count (since `ReplicaDesired` revision changes).

Health-failure path:

1. `health.go` observes `State.Health.Status = unhealthy` → writes `ReplicaObserved{state: degraded}`.
2. Ingress (separate slice) sees this via its Routes/replicas watch and removes the replica from the upstream pool within 5s.
3. Scheduler (separate slice) writes a `ReplicaCommand{replica_id, op: restart}`.
4. `lifecycle.go` consumes the command (subscription on `ReplicaCommand` topic), stops the container and starts a fresh one with the same spec.
5. Health poll resumes; on healthy, normal state.
6. If restart fails (start error from docker, immediate exit), scheduler counts and aborts after 3 consecutive failures.

Log streaming peer RPC:

1. CLI-entry node receives `Deploy.Logs(deployment, service, follow=true)` from the CLI.
2. Entry node enumerates replicas, opens `Internal.Logs(replica_id)` peer RPCs to each runtime hosting a target replica.
3. Runtime peer handler calls `ContainerLogs(ctx, container_id, ShowStdout, ShowStderr, Follow, Since)` against docker.
4. Demuxes via stdcopy.StdCopy or equivalent; emits `LogLine{replica_id, host, stream, ts, line}` per line.
5. Entry node merges streams by arrival; pushes to CLI.

Container update (image change):

1. Watch event: `Event<ReplicaDesired>{Updated, before, after}` with `before.image != after.image`.
2. `lifecycle.go` stops + removes the existing container (no in-place update; docker doesn't support image swap on a running container).
3. Restarts via the create path; same replica id reused; raft_index label updated.
4. `ReplicaObserved` cycles `running → stopped → pending → pulling? → running`.

## §6 Out of scope

- Cross-host volume sync (spec §3 Out).
- Image building (spec §3 Out).
- Container resource quotas beyond per-replica CPU/memory limits (the runtime now projects compose `deploy.resources` and the legacy top-level keys into docker `container.Resources`; `ulimits`/`tmpfs` unchanged). IO/block-device limits remain out of scope (spec §3 Out).
- Network setup beyond attaching to the JACO-managed bridge (lives in discovery slice).
- TLS cert mounting into containers (no spec promise; certs are for ingress only).
- Authoritative scheduling decisions (lives in scheduler slice).
- Cross-node log fanout merge — runtime serves single-replica streams; merge happens in the entry node (cli slice + control-plane slice).

> If the parent spec is ambiguous on anything this slice depends on, stop and update the spec. Do not invent behavior here.
