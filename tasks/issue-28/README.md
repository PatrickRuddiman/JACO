Parent slice(s): [control-plane](../../slices/issue-28/control-plane.md), [datapath](../../slices/issue-28/datapath.md), [dns](../../slices/issue-28/dns.md)

# Issue #28 — Cross-host service discovery — Tasks

Per-host `/24` IPAM with WG-routed cross-host traffic, fixing the colliding-IP service-discovery failure described in [issue #28](https://github.com/PatrickRuddiman/jaco/issues/28).

| # | Task | Path | Depends on | Slice |
|---|------|------|------------|-------|
| 00 | proto-state-ipam-foundation | [00-proto-state-ipam-foundation.md](00-proto-state-ipam-foundation.md) | — | control-plane |
| 01 | fsm-apply-and-free-cascades | [01-fsm-apply-and-free-cascades.md](01-fsm-apply-and-free-cascades.md) | 00 | control-plane |
| 02 | ensure-subnet-rpc | [02-ensure-subnet-rpc.md](02-ensure-subnet-rpc.md) | 00, 01 | control-plane |
| 03 | reconciler-lazy-alloc | [03-reconciler-lazy-alloc.md](03-reconciler-lazy-alloc.md) | 02 | control-plane |
| 04 | boot-migration-purge | [04-boot-migration-purge.md](04-boot-migration-purge.md) | 01 | control-plane |
| 05 | bridge-mtu | [05-bridge-mtu.md](05-bridge-mtu.md) | — | datapath |
| 06 | wg-allowedips-and-routes | [06-wg-allowedips-and-routes.md](06-wg-allowedips-and-routes.md) | 00 | datapath |
| 07 | firewall-set-grouping | [07-firewall-set-grouping.md](07-firewall-set-grouping.md) | — | datapath |
| 08 | snat-return-reconcile | [08-snat-return-reconcile.md](08-snat-return-reconcile.md) | 07 | datapath |
| 09 | health-poll-ip-population | [09-health-poll-ip-population.md](09-health-poll-ip-population.md) | — | dns |
| 10 | dns-manager-bind-locality | [10-dns-manager-bind-locality.md](10-dns-manager-bind-locality.md) | 09 | dns |
| 11 | responder-internal-names | [11-responder-internal-names.md](11-responder-internal-names.md) | — | dns |
| 12 | networkconnect-aliases | [12-networkconnect-aliases.md](12-networkconnect-aliases.md) | — | dns |
| 13 | discovery-real-engine-integration | [13-discovery-real-engine-integration.md](13-discovery-real-engine-integration.md) | 03, 05, 07, 10, 11, 12 | all |

## Dependency graph
```
00 ─┬─> 01 ─┬─> 02 ──> 03 ─┐
    │       └─> 04         │
    └─> 06  (cross-host)   │
                           │
05 ────────────────────────┤
07 ──> 08  (cross-host)    ├─> 13
                           │
09 ──> 10 ─────────────────┤
11 ────────────────────────┤
12 ────────────────────────┘
```

Notes:
- **Independent starters (no deps):** 00, 05, 07, 09, 11, 12 — parallelizable from the start. 00 is the foundation the rest of control-plane + 06 build on.
- **06 (routes/AllowedIPs)** and **08 (SNAT)** are cross-host behaviors; their runtime effect is verified by the privileged 3-node isolation rig (`make test-isolation`), not by the single-host capstone 13.
