Parent slices: [control-plane](../slices/control-plane.md), [cli](../slices/cli.md), [scheduler](../slices/scheduler.md), [runtime](../slices/runtime.md), [discovery](../slices/discovery.md), [ingress](../slices/ingress.md), [packaging](../slices/packaging.md)

# JACO — Tasks

| #  | Task                                                                              | Path                                                                  | Depends on        | Slice         |
|----|-----------------------------------------------------------------------------------|-----------------------------------------------------------------------|-------------------|---------------|
| 00 | project-scaffolding                                                               | [00-project-scaffolding.md](00-project-scaffolding.md)                | —                 | cli           |
| 01 | proto-definitions                                                                 | [01-proto-definitions.md](01-proto-definitions.md)                    | 00                | control-plane |
| 02 | raft-node                                                                         | [02-raft-node.md](02-raft-node.md)                                    | 00, 01            | control-plane |
| 03 | entity-stores-and-watch                                                           | [03-entity-stores-and-watch.md](03-entity-stores-and-watch.md)        | 02, 01            | control-plane |
| 04 | fsm-apply                                                                         | [04-fsm-apply.md](04-fsm-apply.md)                                    | 03                | control-plane |
| 05 | cluster-ca-and-bootstrap                                                          | [05-cluster-ca-and-bootstrap.md](05-cluster-ca-and-bootstrap.md)      | 04                | control-plane |
| 06 | grpc-server-and-admission                                                         | [06-grpc-server-and-admission.md](06-grpc-server-and-admission.md)    | 05                | control-plane |
| 07 | node-join-and-membership                                                          | [07-node-join-and-membership.md](07-node-join-and-membership.md)      | 06                | control-plane |
| 08 | token-rpcs-and-cli                                                                | [08-token-rpcs-and-cli.md](08-token-rpcs-and-cli.md)                  | 06                | control-plane |
| 09 | audit-log                                                                         | [09-audit-log.md](09-audit-log.md)                                    | 06                | control-plane |
| 10 | backup-restore                                                                    | [10-backup-restore.md](10-backup-restore.md)                          | 04, 06            | control-plane |
| 11 | cli-client-scaffold                                                               | [11-cli-client-scaffold.md](11-cli-client-scaffold.md)                | 06                | cli           |
| 12 | cli-output-renderers                                                              | [12-cli-output-renderers.md](12-cli-output-renderers.md)              | 11                | cli           |
| 13 | compose-parser-and-container-spec                                                 | [13-compose-parser-and-container-spec.md](13-compose-parser-and-container-spec.md) | 01                | runtime       |
| 14 | deploy-rpcs                                                                       | [14-deploy-rpcs.md](14-deploy-rpcs.md)                                | 04, 13            | control-plane |
| 15 | deploy-cli-subcommands                                                            | [15-deploy-cli-subcommands.md](15-deploy-cli-subcommands.md)          | 12, 14            | cli           |
| 16 | docker-client-and-pull                                                            | [16-docker-client-and-pull.md](16-docker-client-and-pull.md)          | 04, 13            | runtime       |
| 17 | container-lifecycle                                                               | [17-container-lifecycle.md](17-container-lifecycle.md)                | 16                | runtime       |
| 18 | healthcheck-observation                                                           | [18-healthcheck-observation.md](18-healthcheck-observation.md)        | 17                | runtime       |
| 19 | logs-streaming                                                                    | [19-logs-streaming.md](19-logs-streaming.md)                          | 17, 12            | runtime       |
| 20 | placement-and-counter                                                             | [20-placement-and-counter.md](20-placement-and-counter.md)            | 04, 14            | scheduler     |
| 21 | scheduler-reconcile                                                               | [21-scheduler-reconcile.md](21-scheduler-reconcile.md)                | 20, 18            | scheduler     |
| 22 | rollout-plan                                                                      | [22-rollout-plan.md](22-rollout-plan.md)                              | 21                | scheduler     |
| 23 | health-restart-and-drain                                                          | [23-health-restart-and-drain.md](23-health-restart-and-drain.md)      | 22                | scheduler     |
| 24 | status-rpc-and-cli                                                                | [24-status-rpc-and-cli.md](24-status-rpc-and-cli.md)                  | 21, 12            | cli           |
| 25 | subnet-allocator                                                                  | [25-subnet-allocator.md](25-subnet-allocator.md)                      | 04, 14            | discovery     |
| 26 | wireguard-mesh                                                                    | [26-wireguard-mesh.md](26-wireguard-mesh.md)                          | 07, 25            | discovery     |
| 27 | bridges-and-attach-helper                                                         | [27-bridges-and-attach-helper.md](27-bridges-and-attach-helper.md)    | 25, 17            | discovery     |
| 28 | nftables-ruleset                                                                  | [28-nftables-ruleset.md](28-nftables-ruleset.md)                      | 25                | discovery     |
| 29 | dns-responder                                                                     | [29-dns-responder.md](29-dns-responder.md)                            | 27, 18            | discovery     |
| 30 | isolation-reconcile-loop                                                          | [30-isolation-reconcile-loop.md](30-isolation-reconcile-loop.md)      | 28, 09            | discovery     |
| 31 | isolation-ci-test-rig                                                             | [31-isolation-ci-test-rig.md](31-isolation-ci-test-rig.md)            | 27, 28, 29, 30    | discovery     |
| 32 | embedded-caddy-boot                                                               | [32-embedded-caddy-boot.md](32-embedded-caddy-boot.md)                | 04, 14            | ingress       |
| 33 | certmagic-raft-storage                                                            | [33-certmagic-raft-storage.md](33-certmagic-raft-storage.md)          | 32                | ingress       |
| 34 | acme-issuance-and-rebuild                                                         | [34-acme-issuance-and-rebuild.md](34-acme-issuance-and-rebuild.md)    | 33, 18            | ingress       |
| 35 | release-pipeline                                                                  | [35-release-pipeline.md](35-release-pipeline.md)                      | 00                | packaging     |
| 36 | install-and-systemd                                                               | [36-install-and-systemd.md](36-install-and-systemd.md)                | 35                | packaging     |
| 37 | self-upgrade                                                                      | [37-self-upgrade.md](37-self-upgrade.md)                              | 35, 36            | packaging     |

## Dependency graph

See [dependency-graph.md](dependency-graph.md) for the ASCII graph + full adjacency table.

```
00 ──> 01 ──┬─> 02 ──> 03 ──> 04 ──┬─> 05 ──> 06 ──┬─> 07 ──> 08
            │                      │                ├─> 09
            │                      │                ├─> 10
            │                      │                ├─> 11 ──> 12
            │                      │                │
            └─> 13 ─────────────┐  │                │
                                ▼  ▼                │
                               14 (also <── 04)     │
                                │                   │
              ┌─────────────────┼─────────────────┐ │
              ▼                 ▼                 ▼ ▼
             20                15                32  16 ──> 17 ──┬─> 18
             │                 │                 │              ├─> 19
             ▼                 ▲                 ▼              │
             21 ──> 22 ──> 23  │                33              │
             │                 │                 │              │
             └─> 24 <───── 12 ─┘                 ▼              │
                                                 34 <── 18 ─────┘

             25 ──┬─> 26 (also <── 07)
                  ├─> 27 (also <── 17) ──┬─> 29 (also <── 18)
                  └─> 28 ────────────────┴─> 30 (also <── 09) ──> 31

00 ──> 35 ──> 36 ──> 37
```
