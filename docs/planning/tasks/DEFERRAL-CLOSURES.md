# Deferral closures

Each task file's `## Tasks` section ticks `[x]` as work lands. Some
ship with a `**Deferred**: …` bullet that flags work intentionally
postponed (typically waiting for the daemon entry, task 38, to land).
Those bullets stay unticked in the original task file as historical
record — this index tracks where each got closed.

## Closures by task (chronological)

| Original deferral | Closed by | Commit |
|-------------------|-----------|--------|
| 07: cluster-join.sh E2E | scripts/test/cluster-join.sh | d2637e8 |
| 09: 3-node audit E2E | scripts/test/apply-deploy.sh + isolation-rig.sh | 83c35f5 |
| 15: apply-deploy.sh E2E | scripts/test/apply-deploy.sh | 83c35f5 |
| 17: orphan reconcile into jaco serve | reconciler.bootSweep at startup | be470a3 |
| 17: docker-tagged lifecycle test | internal/runtime/lifecycle/lifecycle_integration_test.go | 813ebe6 |
| 18: docker-tagged health watcher test | internal/runtime/health/health_integration_test.go | d2637e8 |
| 19: Internal.Logs + Deploy.Logs cross-host fanout | iter 28 of task 38 | 72bdfeb |
| 19: logs-fanout.sh E2E | scripts/test/logs-fanout.sh | 83c35f5 |
| 19: docker-tagged logs test | internal/runtime/logs/logs_integration_test.go | d2637e8 |
| 21: scheduler-spread.sh E2E | scripts/test/scheduler-spread.sh | d2637e8 |
| 22: rollout integration into scheduler.Reconcile | task 39 (driveRollout) | c8f7d29 |
| 22: rollout abort under live reconcile | TestReconcile_RolloutAbortsOnStepTimeout | c8f7d29 |
| 23: drain step machine wired to NodeRemove | iter 25 of task 38 | cfb65b1 |
| 23: drain-node.sh E2E | scripts/test/drain-node.sh | 83c35f5 |
| 24: status-watch.sh E2E | scripts/test/status-watch.sh | 83c35f5 |
| 25: Subnet hook into Deploy.Apply | enumerateNetworks + ipam.EnsureSubnets | d2637e8 |
| 26: EnsureInterface + ReconcilePeers + Cluster.UpdateSelf | task 11 (this loop) | 24a2ccd |
| 26: wireguard-tagged real-engine test | internal/discovery/wgmesh/wgmesh_integration_test.go | 813ebe6 |
| 27: container /etc/resolv.conf wiring | task 10 (this loop) | 486f9ba |
| 27: docker-tagged bridge attach test | lifecycle_integration_test.go covers it | 813ebe6 |
| 28: daemon startup nft render+apply before sd_notify | sd_notify in cmd/jacod + iter 17 wiring | d2637e8 |
| 29: per-bridge DNS listener + reconciler + manager | iter 31 of task 38 (dns.Manager) | 5191333 |
| 30: CheckIsolationAvailable in lifecycle.Start | task 9 (this loop) | 66a6d5b |
| 30: nftables-tagged reconcile test | firewall_integration_test.go | 813ebe6 |
| 30: isolation-drift.sh E2E | scripts/test/isolation-drift.sh | 83c35f5 |
| 31: isolation-rig.sh test bodies | scripts/test/isolation-rig.sh rewrite | 83c35f5 |
| 32: caddy.Load embedded Ingress runner | ingressLoaderEmbedded | d8ae176 |
| 33: CertBlob raft-backed blob storage | task 40 | aa9c4b8 |
| 33: caddy.RegisterModule(JacoStorage) | ingress/storage/caddy_module.go | d8ae176 |
| 34: CertMagic OnEvent audit hooks | challenge.Issuer emit | aa9c4b8 |
| 34: Pebble integration test | acme_integration_test.go + scripts/test/ingress-acme.sh | 813ebe6 / 83c35f5 |
| 36: install.sh privileged container CI | .github/workflows/integration.yml | 168eb76 |
| 37: self-upgrade restart + poll + rollback | postUpgradeRestart | 5b978ac |
| 37: scripts/test/self-upgrade.sh | scripts/test/self-upgrade.sh | 5b978ac |
| 38: rollout state machine (full) | task 39 | c8f7d29 |
| 38: TLS-with-cluster-CA | task 41 | 2039d52 |
| 38: real-engine integration tests | task 42 | 813ebe6 |
| 38: Internal.Submit follower forwarding | iter 24 of task 38 | a214972 |

## Open

None tracked here. Future work belongs in new tasks (NN ≥ 43).
