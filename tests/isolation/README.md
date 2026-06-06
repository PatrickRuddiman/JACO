# isolation — network-isolation e2e rig fixtures

Manifest fixtures for JACO's **network-isolation invariants** — the guarantees
that per-`(deployment, network)` bridges + nftables enforce. Consumed by the
privileged 3-node rig (`make test-isolation` →
`scripts/test/isolation-rig.sh`); the `isolation-rig` CI workflow re-runs when
anything here changes.

## What they encode

Two independent deployments, each with two services on two separate networks:

| deployment  | svc-a (net-a, :9999) | svc-b (net-b, :9998) |
|-------------|----------------------|----------------------|
| `dep-front` | busybox `nc` listener (×2) | busybox `nc` listener (×2) |
| `dep-back`  | busybox `nc` listener (×2) | busybox `nc` listener (×2) |

Each service runs `busybox sh -c 'nc -lk -p <port> -e /bin/sh'` — a trivial TCP
listener that gives the rig a target to probe (exec into a peer, `nc` the
listener, observe connect/refuse).

## The invariants under test

- **Same `(deployment, network)`** → reachable. `dep-front/svc-a` replicas can
  reach each other on `net-a`.
- **Different network, same deployment** → blocked. `net-a` ✗ `net-b` within
  `dep-front`.
- **Different deployment** → blocked, even across identically-named networks.
  `dep-front` ✗ `dep-back` (both declare `net-a`/`net-b`, but the bridges and
  subnets are distinct per deployment).

`replicas: 2` forces the services to spread across nodes, so the rig also
exercises **cross-host** reachability (WireGuard mesh) within an allowed
network, not just same-host bridge traffic.

## Files

```
tests/isolation/
├── dep-front.jaco.yaml      # deployment dep-front: svc-a(net-a), svc-b(net-b)
├── dep-front.compose.yml    # the two busybox nc listeners + net-a/net-b
├── dep-back.jaco.yaml       # deployment dep-back: same shape, isolated
└── dep-back.compose.yml
```

## Note on the rig

`scripts/test/isolation-rig.sh` currently **regenerates equivalent manifests
inline** (heredocs into its work dir) rather than reading these files, so today
they serve as the canonical, reviewable reference and the CI path-trigger for
the rig. Pointing the rig at these files directly (single source of truth) is a
worthwhile follow-up — until then, keep the two in sync when editing either.

## Volume isolation (separate fixture, not yet in the rig)

JACO's per-deployment scoping for **named volumes** uses the same
`(deployment, key)` decomposition this rig probes for networks. The
manual smoke fixtures for that invariant live alongside the bench
sample at
[`tests/samples/jaco/smoke-volumes/`](../samples/jaco/smoke-volumes/README.md);
promoting that probe into this privileged rig is a worthwhile
follow-up.