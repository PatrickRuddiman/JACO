Parent slice(s): [TCP ingress — control-plane](../../slices/issue-37/control-plane.md), [TCP ingress — datapath](../../slices/issue-37/datapath.md)

# TCP ingress (issue #37) — Tasks

| # | Task | Path | Depends on | Slice |
|---|------|------|------------|-------|
| 00 | TCPRoute entity plumbing (proto + state store + watch broker) | [00-tcproute-entity-plumbing](00-tcproute-entity-plumbing.md) | — | control-plane |
| 01 | FSM replace-set apply, delete cascade, snapshot | [01-fsm-apply-prune-and-cascade](01-fsm-apply-prune-and-cascade.md) | 00 | control-plane |
| 02 | Compose reserved-port (80/443) validation | [02-compose-reserved-port-validation](02-compose-reserved-port-validation.md) | — | control-plane |
| 03 | TCPRoute derivation + port_conflict collision check | [03-tcproute-derivation-and-collision](03-tcproute-derivation-and-collision.md) | 00 | control-plane |
| 04 | caddy-l4 dependency + BuildCaddyConfig layer4 emission | [04-caddy-l4-layer4-config](04-caddy-l4-layer4-config.md) | — | datapath |
| 05 | Daemon TCP projection + bind-probe + loader gate + reload subscription | [05-daemon-tcp-builder-and-reload](05-daemon-tcp-builder-and-reload.md) | 00, 04 | datapath |

## Dependency graph
```
00 ──┬─> 01
     └─> 03

04 ──┐
00 ──┴─> 05

02   (independent — pure raw-YAML validation)
```
