Parent slice: [discovery](../slices/discovery.md)
Depends on: 27, 28, 29, 30

# Task 31 — isolation-ci-test-rig

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Mandatory CI test rig that validates the cross-deployment + compose-network isolation v1 promise end-to-end on a multi-node cluster.

## Tasks
- [ ] Create `scripts/test/isolation-rig.sh` orchestrating a 3-node cluster — implementation choice: rootful systemd-nspawn OR privileged docker-in-docker with `--cap-add=NET_ADMIN --cap-add=NET_RAW --device /dev/net/tun`.
- [ ] Cluster topology: 3 jaco daemons, kernel WG available, nftables ≥1.0, docker engine reachable.
- [ ] Apply two test deployments (`testdata/isolation/dep-front.jaco.yaml`, `testdata/isolation/dep-back.jaco.yaml`), each with two compose-networks (`net-a`, `net-b`); each service runs `busybox sh -c 'nc -lk -p 9999'`.
- [ ] **Positive**: exec into a container in `dep-front/net-a` on node 1, `nc -w 3 <peer-replica-ip> 9999` to a peer in `dep-front/net-a` on node 3 → success; same for UDP via `nc -u`; `dig +short` of the service returns IPs.
- [ ] **Negative**: cross-deployment `nc -w 3` → timeout (exit 1); UDP same; DNS lookup of `dep-back-service` from `dep-front` container returns NXDOMAIN.
- [ ] **Cross-network within deployment**: `dep-front/net-a` → `dep-front/net-b` service: DNS NXDOMAIN; by-IP `nc` → drop.
- [ ] **Drift recovery**: harness shell-calls `nft flush table inet jaco` on node 2; within 30s `jaco audit --type isolation_ruleset_reconciled` returns ≥1 event; positive test re-runs and succeeds.
- [ ] **Startup failure**: relaunch one daemon with `nft` removed from PATH (or `--simulate-isolation-failure` flag added behind a build tag); assert daemon never reaches systemd-ready; `jaco status -o json | jq '.nodes[] | select(.hostname=="failed-node") | .status'` prints `"isolation_unavailable"`.
- [ ] Add `make test-isolation` Makefile target invoking the rig.
- [ ] Add GitHub Actions CI job `isolation-rig` that runs `make test-isolation` on every PR touching `internal/discovery/`, `internal/runtime/`, `spec.md`, `design.md`, or `slices/discovery.md`.

## Acceptance criteria
- [ ] `make test-isolation` exits 0 in CI.
- [ ] Rig stdout contains `PASS: positive same-net cross-node`, `PASS: negative cross-deployment`, `PASS: negative cross-network`, `PASS: drift recovery`, `PASS: startup failure`.
- [ ] CI job `isolation-rig` is configured (`grep -l isolation-rig .github/workflows/`).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
