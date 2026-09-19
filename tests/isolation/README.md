# isolation — network-isolation e2e rig fixtures

Manifest fixtures for JACO's **network-isolation invariants** — the guarantees
that per-`(deployment, network)` bridges + nftables enforce. Consumed by the
privileged 3-node rig (`make test-isolation` →
`scripts/test/isolation-rig.sh`); the `isolation-rig` CI workflow re-runs when
anything here changes.

## What they encode

Two independent deployments, each with two services on two separate networks:

| deployment  | svc-a (private-net, :9999) | svc-b (private.net, :9998) |
|-------------|----------------------|----------------------|
| `dep-front` | busybox `nc` listener (×2) | busybox `nc` listener (×2) |
| `dep-back`  | busybox `nc` listener (×2) | busybox `nc` listener (×2) |

Each service runs `busybox sh -c 'nc -lk -p <port> -e /bin/sh'` — a trivial TCP
listener that gives the rig a target to probe (exec into a peer, `nc` the
listener, observe connect/refuse).

`private-net` and `private.net` are distinct valid Compose network names that
the legacy firewall sanitizer mapped to the same nftables identifier. These
fixtures pin that regression: they must remain separate firewall scopes.

## The invariants under test

- **Same `(deployment, network)`** → reachable. `dep-front/svc-a` replicas can
  reach each other on `private-net`.
- **Different network, same deployment** → blocked. `private-net` ✗ `private.net` within
  `dep-front`.
- **Different deployment** → blocked, even across identically-named networks.
  `dep-front` ✗ `dep-back` (both declare `private-net`/`private.net`, but the bridges and
  subnets are distinct per deployment).

`replicas: 2` forces the services to spread across nodes, so the rig also
exercises **cross-host** reachability (WireGuard mesh) within an allowed
network, not just same-host bridge traffic.

## Files

```
tests/isolation/
├── dep-front.jaco.yaml      # svc-a(private-net), svc-b(private.net)
├── dep-front.compose.yml    # the two busybox nc listeners + both networks
├── dep-back.jaco.yaml       # deployment dep-back: same shape, isolated
└── dep-back.compose.yml
```

## Note on the rig

`scripts/test/isolation-rig.sh` currently **regenerates equivalent manifests
inline** (heredocs into its work dir) rather than reading these files, so today
they serve as the canonical, reviewable reference and the CI path-trigger for
the rig. Pointing the rig at these files directly (single source of truth) is a
worthwhile follow-up — until then, keep the two in sync when editing either.

The inline rig uses the same punctuation-colliding network names and probes
the target IPs directly, after checking that each target listener is live.
DNS failures are not accepted as proof of L3 isolation. The dedicated workflow
is currently disabled (`JACO_RIG_FORCE=0`); the live rig still requires a
disposable privileged Linux host. Unit tests cover generated new/established/
related packet verdicts; the gated nftables integration test covers atomic
legacy-set migration in an isolated network namespace.

## Volume isolation (separate fixture, not yet in the rig)

JACO's per-deployment scoping for **named volumes** uses the same
`(deployment, key)` decomposition this rig probes for networks. The
manual smoke fixtures for that invariant live alongside the bench
sample at
[`tests/samples/jaco/smoke-volumes/`](../samples/jaco/smoke-volumes/README.md);
promoting that probe into this privileged rig is a worthwhile
follow-up.