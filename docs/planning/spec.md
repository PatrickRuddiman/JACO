# JACO — Just Another Container Orchestrator

## §1 Summary

Teams running multi-host Docker workloads today choose between Kubernetes (steep learning curve, very large surface area), Docker Swarm (deprecated trajectory), or hand-stitching docker compose with ansible and a reverse proxy. JACO gives those teams a multi-node orchestrator that consumes their existing docker compose files unchanged, adds a small overlay YAML to declare per-service replica counts and host placement, and routes ingress on every node via a single declarative routing block in the same overlay file. The cluster forms a raft consensus group; any node accepts CLI commands, and any node accepts ingress traffic for any declared domain. JACO exists for small-to-medium operators who want declarative multi-node container scheduling without adopting a Kubernetes-shaped platform.

Top-level promises (each measurable):

- A cluster of N nodes tolerates ⌊(N−1)/2⌋ simultaneous node failures without losing the ability to apply new deployments or reschedule containers (externally observable: `jaco apply` succeeds, scheduling continues).
- p95 ingress routing overhead — time from packet arrival at the routing node to upstream container receiving it — is < 5 ms on a LAN within the same datacenter.
- A new raft leader is elected and `jaco apply` is again accepted within 10 s of the previous leader becoming unreachable.
- A jaco.yaml apply that requires no image pull and changes ≤ 10 replicas reaches steady state within 15 s on a 3-node LAN cluster.
- The same docker compose file that runs under `docker compose up` runs under JACO without modification, for every compose v3+ service field listed in §3 In.
- TLS certificates for declared domains are obtained, installed, and renewed without operator action after initial DNS ownership is established.
- Any node in the cluster accepts ingress traffic for any domain declared in any jaco.yaml; no client-side load balancer or external L4 router is required for the cluster to be reachable.

## §2 Behavior

### Personas

- **Operator** — installs JACO on hosts, bootstraps clusters, adds nodes, gracefully removes nodes, monitors cluster health, manages cluster tokens, upgrades JACO on each node, recovers from node failures, backs up and restores cluster state.
- **Application developer** — authors docker-compose.yaml and jaco.yaml pairs, applies them to a cluster, scales services, updates services (image, replica count, host placement, routing), rolls back deployments, deletes deployments, inspects deployment status, streams container logs.
- **End user** — issues HTTP(S) requests to a domain declared in a jaco.yaml. Does not interact with JACO controls. Included because ingress behavior is part of the contract.

### User stories

Operator:

- *As an operator*, I want to bootstrap a single-node cluster so I can start using JACO with one host.
- *As an operator*, I want to join additional nodes to an existing cluster so I can grow capacity.
- *As an operator*, I want to gracefully remove a node so I can decommission a host without dropping traffic.
- *As an operator*, I want to see which node is the current raft leader so I know where writes coordinate.
- *As an operator*, I want to see the status of every node (leader, follower, candidate, joining, leaving, offline) at a glance so I can detect failures.
- *As an operator*, I want JACO to elect a new leader automatically when the current leader fails so I do not need to intervene.
- *As an operator*, I want to upgrade JACO on a single node without bringing down the cluster so I can apply patches safely.
- *As an operator*, I want JACO to obtain and renew TLS certificates for declared domains so I do not manage certificate lifecycle by hand.
- *As an operator*, I want to issue and revoke cluster CLI tokens so I control who can apply changes.
- *As an operator*, I want to back up cluster state and restore it onto a new host so I can recover from total cluster loss.

Application developer:

- *As a developer*, I want JACO to consume my existing docker-compose.yaml unchanged, so I do not rewrite service definitions.
- *As a developer*, I want to declare `replicas: N` per service so I control how many instances run.
- *As a developer*, I want to pin a service to a specific list of node hostnames so I can place stateful or hardware-bound workloads.
- *As a developer*, I want to declare domain → service routing in jaco.yaml so ingress and deployment are one document.
- *As a developer*, I want to apply a jaco.yaml and have JACO create, update, or remove containers to match the declared state.
- *As a developer*, I want to scale a service up or down by editing replicas and re-applying.
- *As a developer*, I want to update a service's image and re-apply, and have JACO roll the update across replicas without dropping all replicas at once.
- *As a developer*, I want to roll back to the previously applied jaco.yaml for a deployment so I can recover from a bad deploy.
- *As a developer*, I want to see the status of every service in a deployment (pending, running, degraded, updating, failed, stopped) so I know whether the deploy succeeded.
- *As a developer*, I want to stream logs from every replica of a service from any node so I can debug without SSHing to individual hosts.
- *As a developer*, I want services in the same deployment to reach each other by service name from any node so I do not configure cross-node networking by hand.
- *As a developer*, I want to delete a deployment and have its containers, routes, and certificates removed.

End user:

- *As an end user*, I want my HTTPS request to a declared domain to reach a healthy replica of the target service.

### Acceptance criteria

Cluster lifecycle:

- Given no cluster exists, When the operator runs the bootstrap command on host A, Then host A becomes a single-node cluster with itself as raft leader, and `jaco status` from A reports `nodes: 1, leader: A, healthy: true`.
- Given a single-node cluster with host A as leader, When the operator runs the join command on host B targeting host A, Then within 5 s `jaco status` from any node reports `nodes: 2, leader: A, members: [A, B]`.
- Given a 3-node cluster {A, B, C} with A as leader, When host A is powered off, Then within 10 s `jaco status` from B or C reports a new leader (B or C), and `jaco apply` against the surviving leader succeeds.
- Given a 3-node cluster, When the operator runs the graceful-remove command for node C, Then JACO immediately begins placing replacement replicas for C's workloads on A or B (subject to placement constraints); replicas on C continue serving until each replacement passes its health check; then C's replicas are stopped, C is removed from raft membership, and `jaco status` reports `nodes: 2`.
- Given a 2-node cluster (no raft majority possible after one loss), When one node fails, Then `jaco status` from the surviving node reports `leader: none, writes: rejected` and `jaco apply` returns an error stating quorum is lost. Read-only commands (status, logs) continue to function from the surviving node.

Deployment lifecycle:

- Given a docker-compose.yaml defining service `web` (image `nginx:1.25`) and a jaco.yaml declaring `web: replicas: 3, hosts: [A, B, C]`, When the developer runs apply against any node, Then within 15 s (no pull required) three containers running `nginx:1.25` exist — one on each of A, B, C — and `jaco status web` reports `running: 3/3`.
- Given the above deployment, When the developer changes `replicas` to 1 and re-applies, Then two of the three containers are stopped and removed within 10 s and `jaco status web` reports `running: 1/1`.
- Given the above deployment, When the developer changes the image to `nginx:1.26` and re-applies, Then JACO replaces replicas one at a time, never dropping below 2 running replicas during the roll, `jaco status web` reports `updating` during the roll, and `running: 3/3` at the end.
- Given a service running version v2, When the developer runs the rollback command, Then JACO re-applies the previous successful jaco.yaml for that deployment and `jaco status` reports the prior image is running on all replicas.
- Given a jaco.yaml that pins service `db` to host `A`, When applied, Then `db` replicas run only on host A. If A is unreachable, `jaco status db` reports `pending: cannot satisfy host placement: A unreachable` and no replicas are scheduled elsewhere.
- Given a deployment is deleted, When the operation completes, Then all containers for that deployment are stopped, route entries for its domains are removed, certificates for its domains stop being renewed, and `jaco status` no longer lists the deployment.
- Given a jaco.yaml declaring `web: replicas: 0`, When applied, Then no `web` containers run, but the deployment, its declared routes, and its certificates remain provisioned and renewed. When the developer changes `replicas` to 1 and re-applies, `web` is back online within 15 s without re-issuing certificates.

Routing and ingress:

- Given a jaco.yaml that declares `routes: example.com → service web port 80`, When applied, Then any HTTP request to `example.com` arriving at any node in the cluster is forwarded to a healthy replica of `web`. Verify by sending one request to each node's IP with `Host: example.com` — each request returns a response from a `web` replica.
- Given a routes block that declares `tls: auto` for `example.com`, When applied and DNS for `example.com` resolves to one or more cluster node addresses, Then within 60 s a public-CA-issued certificate is installed on every node and `https://example.com` returns the expected response from `web`.
- Given a route to service `web` with three replicas, When one replica fails its health check, Then no new requests are forwarded to that replica until it is healthy again. Existing in-flight requests to the failing replica are allowed to complete.
- Given a route specifying a port that no replica is listening on, When a request arrives, Then the routing node returns HTTP 502 and `jaco status web` reports the unreachable target.

Service discovery:

- Given services `web` and `api` in the same jaco.yaml, When a `web` replica on node A connects to `http://api:<port>`, Then the connection reaches a healthy `api` replica regardless of which node that replica runs on.
- Given two deployments `front` and `back` each running a service `web`, When a container in `front` resolves or connects to `back.web`, Then resolution fails (NXDOMAIN) and any direct-IP connection attempt is blocked at the network layer.
- Given a single deployment whose compose file declares networks `frontend` and `backend`, services `api` on `[backend]`, `web` on `[frontend]`, and `cache` on `[backend]`, When `api` connects to `cache`, Then the connection succeeds. When `web` connects to `api`, Then the connection fails (services on disjoint networks).

Logs and inspection:

- Given a deployment with replicas on nodes A and B, When the developer runs `jaco logs <service>` from any node, Then log lines from every replica across A and B are streamed, each tagged with the replica identifier and host; lines from a single replica appear in their original order, and cross-replica lines are interleaved by arrival time at the streaming node (no global re-ordering or buffering).
- Given the developer runs `jaco status` from any node, Then the output lists every deployment, every service within it, replica counts (desired and running), placement constraints, and route entries.

### Failure modes

- Given the raft leader becomes unreachable, When `jaco apply` is issued during the election window, Then the CLI returns "no leader, retrying" and either succeeds within 10 s once an election completes, or fails with "no leader after 10 s" and exits non-zero.
- Given a node loses network connectivity to the rest of the cluster, When the partition lasts beyond the raft election timeout, Then the minority side reports `leader: none, writes: rejected`; replicas already running on the minority side continue to run; ingress on the minority side continues to serve traffic for replicas still running locally; the minority side accepts no new scheduling.
- Given a docker image pull fails (registry unreachable, auth failure, or image not found), When applying a jaco.yaml that requires that image, Then `jaco status <service>` reports `failed: image pull error: <reason>`, no partial replicas are left running on the new image, and the prior deployment of that service continues to serve traffic unchanged.
- Given a service replica fails its compose-declared healthcheck past the declared threshold, When the failure persists, Then JACO removes that replica from routing within 5 s and restarts it; if restart fails 3 consecutive times the replica is reported `failed` in `jaco status` and is not retried until the next apply. The compose `restart` field is ignored — JACO's scheduler owns restart decisions cluster-wide.
- Given a jaco.yaml references a service name not present in the docker-compose.yaml, When applied, Then the apply is rejected with `unknown service: <name>; declared compose services: [...]` and no state changes.
- Given a jaco.yaml pins a service to a hostname not present in the cluster, When applied, Then the apply is rejected with `unknown host: <name>; cluster members: [...]` and no state changes.
- Given a jaco.yaml requests `replicas: N` for a service pinned to fewer than N hosts, When applied, Then the apply is rejected with `cannot place N replicas on M pinned hosts` and no state changes.
- Given a compose file references a network not declared in its top-level `networks:` block, When applied, Then the apply is rejected with `unknown network: <name>; declared: [...]` and no state changes.
- Given a node's kernel does not support nftables, or the JACO-managed ruleset fails to load at startup, When the daemon attempts to come online, Then it refuses to join the cluster as a ready member; `jaco status` reports `node: <name>: isolation_unavailable: <reason>`; no containers are scheduled on that node; the cluster continues to operate on other nodes.
- Given the JACO-managed nftables ruleset is removed or modified out-of-band by an operator, When JACO's reconcile runs (within 30 s), Then JACO restores its expected ruleset via an atomic reload; an audit event `isolation_ruleset_reconciled` is recorded with the offending diff in `details`.
- Given TLS certificate issuance fails (DNS does not resolve to a cluster node, or the public CA rate-limits the request), When the routing block requests `tls: auto`, Then plaintext HTTP routing for that domain remains active, `jaco status` reports `cert: pending: <reason>`, and JACO retries with exponential backoff capped at 1 hour between attempts.
- Given the operator runs the bootstrap command on a node that is already a cluster member, When the command runs, Then the command is rejected with `node is already a member of cluster <id>` and no state changes.
- Given the operator removes a node that hosts the only replica of a service pinned to that host, When the remove runs without `--force`, Then the remove is rejected with `node hosts pinned replicas: [...]`. When run with `--force`, the replica is stopped and `jaco status` reports the service `pending: cannot satisfy host placement: <host> removed`.
- Given disk on a node fills and the docker daemon cannot start new containers, When scheduling targets that node, Then `jaco status <replica>` reports `failed: docker error: <reason>` and the replica is rescheduled to another eligible node, unless host-pinned, in which case it remains `failed` until the operator intervenes.
- Given the docker daemon on a node is stopped, When `jaco status` is queried, Then that node reports `docker: unreachable` and replicas scheduled there are rescheduled to other eligible nodes; ingress on that node continues to operate for routing decisions whose targets resolve to remote replicas.
- Given a CLI command is issued with a token that has been revoked, When the command runs, Then it is rejected with `token revoked` and no state changes; revocation is in effect cluster-wide within 5 s of the revoke command completing.
- Given two `jaco apply` commands targeting the same deployment arrive at the leader within the same second, When processed, Then they are serialized; the second observes the post-state of the first; neither is silently dropped.

## §3 Scope

In:

- Multi-node clusters formed via raft consensus, with any odd number of nodes ≥ 1.
- Cluster bootstrap, node join, graceful node remove, automatic leader election, manual leader-step-down.
- Consumption of unmodified docker compose schema-v3+ files. The supported compose service-level fields are: `image`, `command`, `entrypoint`, `environment`, `env_file`, `volumes` (named volumes and host bind mounts), `ports` (documentation only — ingress is controlled by the jaco routes block), `depends_on` (ordering only), `healthcheck`, `labels`, `user`, `working_dir`, `tmpfs`, `cap_add`, `cap_drop`, `sysctls`, `ulimits`, `read_only`, `networks`. The top-level compose `networks:` block (for declaring networks) is also honored.
- Cross-deployment network isolation: containers in two different deployments cannot reach each other at the network layer. Enforced cluster-wide.
- Within a deployment, compose `networks:` semantics are honored cluster-wide: a service is reachable from another service only when they share at least one declared network. Services that declare no networks attach to a per-deployment default network.
- The supported jaco.yaml fields are: `deployment` (deployment name), `services` (list of `{name, replicas: int ≥ 0, hosts: [hostname, …], placement: spread | pack | hosts, compose_service, networks: [name, …]}`; `placement: hosts` requires a non-empty `hosts`; the other placements ignore it; if `placement` is omitted it defaults to `spread`; if `compose_service` is omitted it defaults to `name`), `routes` (list of `{domain, service, port, tls: auto | off, path: optional URL path prefix}`; `path` defaults to `""` (catch-all); multiple routes for the same domain are permitted when their paths differ — longer prefixes take priority). The matching docker compose file is supplied alongside the jaco.yaml (auto-discovered as `compose.yml`/`compose.yaml` next to the manifest, or passed explicitly to the CLI), not embedded as a jaco.yaml field.
- Service discovery: replicas of services in the same deployment resolve each other by service name from any node.
- Ingress on every node: any node accepts HTTP and HTTPS traffic for any declared domain and forwards to a healthy replica anywhere in the cluster.
- Automatic TLS certificate provisioning and renewal for declared domains from a public CA via the ACME protocol when `tls: auto`.
- Rolling updates: replicas are replaced one at a time; at most one replica per service is down during the roll.
- Rollback to the previously applied jaco.yaml for a given deployment name.
- CLI commands are accepted by any node; non-leader nodes forward writes to the current leader transparently.
- `jaco apply --dry-run` prints the diff between current cluster state and the proposed jaco.yaml and exits without applying. Apply without the flag is the default action.
- Cluster state backup (export to a single file) and restore (import on a fresh host).
- Streaming logs from every replica of a service from any node.
- Per-cluster CLI tokens, each bound to an operator-chosen identity name (e.g. `alice`, `ci-deploy`). Tokens are issued at bootstrap and via an operator command thereafter; tokens may be revoked. Every state-changing CLI command attributes its action to the identity on the presented token.
- An audit log of cluster-state-changing events readable from any node, queryable by time range (e.g. `--since 1h`) and event type (e.g. `--type apply,delete`).

Out:

- Auto-scaling based on metrics. Replica counts are declarative only.
- Cross-cluster federation. One cluster is one raft group.
- Any compose field not listed in §3 In. Fields outside that set cause apply to be rejected with an explicit list of unsupported fields.
- Docker Swarm or Kubernetes API parity, or any migration shim from either.
- Mutual TLS between end users and ingress. Ingress accepts plaintext HTTP and public-CA HTTPS only.
- Image building. JACO consumes pre-built images from registries.
- Persistent-volume replication across nodes. Named volumes live on the node where the replica runs. Host pinning is the operator's tool for stickiness.
- An L4 load balancer or DNS service in front of the cluster. Operators are responsible for pointing DNS at one or more cluster node addresses.
- Authentication of end-user HTTP traffic. JACO routes; it does not authenticate ingress.
- A web UI in v1. All cluster and deployment operations are CLI-only.
- Resource quotas beyond per-replica CPU/memory limits. JACO honors compose `deploy.resources.{limits,reservations}` and the legacy top-level keys (`cpus`, `mem_limit`, `mem_reservation`, `pids_limit`, `cpu_shares`, `cpuset`), plus `ulimits`/`tmpfs`. Still out of scope: IO/block-device limits, and capacity-aware scheduling — per-container limits constrain a replica but do not prevent the cluster from overcommitting a node.
- Secret management beyond what compose `environment` and `env_file` provide.

## §4 Quality bars

Performance:

- p95 ingress routing overhead < 5 ms on a LAN within the same datacenter.
- New raft leader elected within 10 s of the previous leader becoming unreachable.
- A jaco.yaml apply with no image pulls and ≤ 10 replica changes reaches steady state within 15 s on a 3-node LAN cluster.
- `jaco status` returns within 1 s on a cluster with ≤ 100 deployments and ≤ 1000 replicas.

Reliability:

- An N-node cluster tolerates ⌊(N−1)/2⌋ simultaneous node failures with no loss of write availability.
- Surviving nodes continue serving ingress for replicas still running on them during a network partition.
- Cluster state is durable across full-cluster restart: a cluster cleanly stopped and started returns to the same declared state and same set of running replicas.
- Restored cluster state from a backup taken at time T contains every applied deployment that committed before T and no deployment that committed after T.

Security:

- CLI access requires a per-cluster token. Each token is bound to a single identity name chosen by the operator at issue time; tokens are not reusable across identities. The operator may issue additional tokens and revoke any token. Identity names are not authenticated against an external directory; the operator is the source of truth for which name to issue.
- Token revocation is effective cluster-wide within 5 s of the revoke command completing.
- TLS private keys for declared domains never leave the cluster.
- Containers in different deployments cannot reach each other at the network layer. Enforced cluster-wide by both (a) per-(deployment, network) bridge separation and (b) JACO-managed nftables FORWARD rules. Both mechanisms are required to be operational on every cluster node before that node is considered ready.
- Containers within the same deployment but on disjoint compose `networks:` cannot reach each other. Enforced by the same mechanism as cross-deployment isolation.
- The only fields permitted in a jaco.yaml are: `deployment`, `services`, `routes`. The only sub-fields permitted under `services[*]` are: `name`, `replicas`, `placement`, `hosts`, `compose_service`, `networks`. The only sub-fields permitted under `routes[*]` are: `domain`, `service`, `port`, `tls`, `path`. Unknown fields cause apply to be rejected.
- The only events recorded in the cluster audit log are: cluster bootstrap, node join, node remove, leader election, apply (deployment name, identity name, success/failure, diff summary), rollback (deployment name, identity name), delete (deployment name, identity name), certificate obtained, certificate renewed, certificate failed, token issued (identity name), token revoked (identity name), subnet allocated (deployment, network, cidr), subnet released (deployment, network, cidr), isolation ruleset reconciled (node, diff).

Compatibility:

- Runs on Linux hosts with Docker Engine ≥ 24.0.
- Consumes docker compose files at schema versions 3.0 through the current docker compose major version at the time of each JACO release.
- Operator and developer CLI runs on Linux and macOS, x86_64 and arm64.

> If any requirement is ambiguous, stop and ask. Before producing a plan, list your assumptions and the implementation choices you intend to make. Do not write code until those are confirmed.
