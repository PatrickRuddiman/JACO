# smoke-volumes — per-deployment volume-isolation probe

Fixture pair for the live smoke test that proves JACO scopes named
volumes per deployment (`jaco_<deployment>_<key>`). Until this is
promoted into the privileged `tests/isolation` rig, it lives here as
the reviewable artifact for the manual smoke run documented in the PR
that introduces the change.

## The three invariants under test

1. **Default isolation.** Two deployments declaring the same bare
   volume key (`data`) on the same host produce two distinct docker
   volumes (`jaco_vol-front_data` and `jaco_vol-back_data`) — they do
   NOT silently share storage.
2. **Disjoint backing storage.** Writing a sentinel into one volume
   does NOT appear under the same path in the other. This is the
   teeth of invariant (1) — the names alone are not enough; the
   bytes must be separate.
3. **Opt-out parity.** When the operator sets
   `volumes.<key>.name: <literal>` at the compose top level, JACO uses
   the literal docker volume name verbatim (unprefixed). This is the
   compose-portable escape hatch for sharing storage across stacks.

## Files

```
tests/samples/jaco/smoke-volumes/
├── front.jaco.yaml      # deployment vol-front, pinned to jaco-1
├── front.compose.yml    # redis + named volume `data`, no name override
├── back.jaco.yaml       # deployment vol-back,  pinned to jaco-1 (co-located)
├── back.compose.yml     # redis + named volume `data`, no name override
└── shared.compose.yml   # redis + `volumes.data.name: smoke-shared-data`
```

Both deployments pin to `jaco-1` because the bug this change fixes —
two deployments mounting the same docker volume — can only manifest
when both end up on the same docker daemon. Without pinning, the
scheduler may place them on different nodes and hide the collision
behind per-node state fragmentation.

`redis:7-alpine` is the workload: small, fast to start, no application
data, and a sentinel file under `/data` is enough to prove the
invariant.

## How the smoke is run

The full sequence lives in the PR introducing per-deployment volume
scoping (look for the "Phase C — Azure live smoke test" section). In
brief, on a 3-node testbed brought up by
`tests/testbed/scripts/deploy.sh` plus
`tests/samples/jaco/bootstrap/bootstrap.sh`. Capture the leader
address and operator token from cluster init — `jaco delete` is
admin-only and dials `--server` directly (no local-socket
shortcut), so the runbook needs both:

```sh
LEADER=<node-1 private IP>:7000     # e.g. 172.16.0.6:7000
TOKEN=<operator_token printed by `jaco cluster init`>

# Ship and apply both probe deployments to node-1.
scp tests/samples/jaco/smoke-volumes/*.{yaml,yml} \
    azureuser@<n1>:/home/azureuser/smoke/
ssh azureuser@<n1> 'sudo jaco apply ~/smoke/front.jaco.yaml \
                          --compose ~/smoke/front.compose.yml'
ssh azureuser@<n1> 'sudo jaco apply ~/smoke/back.jaco.yaml \
                          --compose ~/smoke/back.compose.yml'

# Probe containers start fast (~5s) but `jaco status` reports PENDING
# indefinitely because redis:7-alpine has no compose `healthcheck:`
# and the daemon needs one to transition to RUNNING. Skip `jaco
# status` — go straight to the docker layer:
ssh azureuser@<n1> 'sudo docker ps --format "{{.Names}}\t{{.Status}}" | grep vol-'
#   expected (both Up):  jaco_vol-front-redis-0   Up …
#                        jaco_vol-back-redis-0    Up …

# Invariant (1) — distinct, deployment-scoped docker volumes:
ssh azureuser@<n1> 'sudo docker volume ls --format "{{.Name}}" | grep _data'
#   expected: jaco_vol-front_data
#             jaco_vol-back_data
#   (NO bare `data` line)

# Invariant (2) — disjoint backing storage:
ssh azureuser@<n1> 'sudo docker run --rm \
  -v jaco_vol-front_data:/d busybox sh -c "echo front > /d/who"'
ssh azureuser@<n1> 'sudo docker run --rm \
  -v jaco_vol-back_data:/d busybox sh -c "cat /d/who 2>/dev/null \
      && echo COLLISION || echo isolated"'
#   expected: isolated     (COLLISION = hard fail)

# Invariant (3) — top-level `name:` opt-out. Re-applying with a
# different compose does NOT trigger a container roll on its own
# (the volume rename isn't part of the rollout key), so DELETE the
# probe first, then re-apply with shared.compose.yml.
ssh azureuser@<n1> "sudo jaco delete vol-front --server $LEADER --token $TOKEN"
ssh azureuser@<n1> 'sudo jaco apply ~/smoke/front.jaco.yaml \
                          --compose ~/smoke/shared.compose.yml'
# Wait ~5s for the new replica to come up:
ssh azureuser@<n1> 'sudo docker inspect jaco_vol-front-redis-0 \
                          --format "{{(index .Mounts 0).Name}}"'
#   expected: smoke-shared-data   (no jaco_ prefix)
ssh azureuser@<n1> 'sudo docker volume ls --format "{{.Name}}" | grep smoke-shared-data'
#   expected: smoke-shared-data
```

Tear-down (admin endpoint, both flags required):

```sh
ssh azureuser@<n1> "sudo jaco delete vol-front --server $LEADER --token $TOKEN"
ssh azureuser@<n1> "sudo jaco delete vol-back  --server $LEADER --token $TOKEN"
# Container removal does NOT cascade-delete its volume (docker
# semantics). Prune the probe volumes for a clean bed:
ssh azureuser@<n1> 'sudo docker volume rm \
  jaco_vol-front_data jaco_vol-back_data smoke-shared-data'
```

The bed itself and the bench workload alongside are left in place
for the next smoke.

## Relation to the network-isolation rig

The privileged 3-node rig under
[`tests/isolation/`](../../isolation/README.md) covers network-level
isolation (L2 bridges + nftables) for the same "two deployments,
identical bare names" shape this fixture probes for volumes. Promoting
the volume invariant into that rig — so it runs in CI alongside the
network invariants — is the next step; until that happens, this
directory is the canonical fixture and the smoke is run manually
against an Azure testbed.
