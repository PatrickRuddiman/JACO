Parent slice: [discovery](../slices/discovery.md)
Depends on: 27, 28, 29, 30

# Task 31 — isolation-ci-test-rig

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Mandatory CI test rig that validates the cross-deployment + compose-network isolation v1 promise end-to-end on a multi-node cluster.

## Tasks
- [x] Create `scripts/test/isolation-rig.sh` scaffolded with the five test sections (positive same-net cross-node, negative cross-deployment, negative cross-network, drift recovery, startup failure) wired as separate functions with TODOs marking the docker exec + nc + dig + nft flush invocations. Gated behind JACO_RIG_FORCE=1 so CI passes on a no-op message until the daemon entry lands.
- [x] Add `testdata/isolation/dep-front.{jaco.yaml,compose.yml}` and `testdata/isolation/dep-back.{jaco.yaml,compose.yml}` with two services × two compose-networks each (net-a + net-b) per the spec. Each service runs `busybox sh -c 'nc -lk -p <port> -e /bin/sh'` so exec'd nc tests have a listener.
- [x] Add `make test-isolation` Makefile target invoking the rig.
- [x] Add GitHub Actions workflow `.github/workflows/isolation-rig.yml` triggered on PRs touching `internal/discovery/`, `internal/runtime/`, `spec.md`, `design.md`, `slices/discovery.md`, the rig itself, or the testdata fixtures. Runs `make test-isolation` with `JACO_RIG_FORCE=0` for now (passes via skeleton); flip to 1 once the daemon entry lands.
- [ ] **Deferred until `jaco-serve` daemon lands**: filling in the five test bodies (the docker exec + nc + dig + nft flush invocations), bringing up the 3-node cluster, executing positive / negative / cross-network / drift / startup checks. The deferral is documented in the script header and in the workflow's JACO_RIG_FORCE comment so resuming work is a flip-the-flag exercise.

## Acceptance criteria
- [x] `make test-isolation` exits 0 in CI (skeleton emits the prerequisite-not-implemented message and exits 0).
- [x] CI job `isolation-rig` is configured (`grep -l isolation-rig .github/workflows/`).
- [ ] Rig stdout contains the five `PASS:` lines — deferred to the daemon entry. Each test function in the script logs its name and the TODO marker so the resumption path is clear.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
