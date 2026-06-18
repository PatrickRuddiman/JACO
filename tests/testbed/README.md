# testbed — Azure provisioning for the orchestrator benchmark

A neutral 3-node Debian substrate that any orchestrator in [`../samples`](../samples)
can be bootstrapped onto, so JACO, Kubernetes (kubeadm), k3s, and Docker Swarm
are compared on identical hardware and network.

## Network model

- **Each VM gets its own public IP.** SSH (22) is reachable directly per node —
  no Tailscale, no VPN, no mesh assumed at provisioning time. This is what lets
  the same bed serve every orchestrator fairly.
- **A persistent Standard Load Balancer** fronts 80 + 443 across all VMs at one
  stable IP (`jaco-lb-pip` in the `jaco-net` RG). Point the `*.jaco.prcs.xyz`
  (and apex `jaco.prcs.xyz`) A record
  at it once; it survives teardowns.

```
                          Internet
             ┌───────────────┴────────────────┐
        per-VM PIP (SSH 22)            LB PIP (80/443, jaco.prcs.xyz)
        ┌────┬────┬────┐                       │
      jaco-1 jaco-2 jaco-3  ◄── LB backend pool (80/443) ──┘
        └─── VNet 172.16.0.0/24 (private mesh/CNI traffic) ───┘
```

The base image is **minimal on purpose** — `cloud-init.yaml.tpl` installs only
OS prep (forwarding sysctls, chrony, curl/jq). No container runtime, no
orchestrator. Each stack's bootstrap installs everything itself from this
identical baseline, so "ease of setup" and "time-to-ready" are measured fairly.

## Files

```
testbed/
├── template.bicep          # VMs, per-VM PIPs, NSG (22/80/443), LB + lb-pip
├── parameters.bicepparam   # secrets sourced from env (see .env.local)
├── cloud-init.yaml.tpl      # minimal base OS prep — no orchestrator
├── .env.local.example       # copy to .env.local and fill in
└── scripts/
    ├── deploy.sh            # provision the bed; prints per-VM PIPs + LB IP
    ├── teardown.sh          # delete the main RG (keeps the persistent LB IP)
    ├── startup.sh           # start deallocated VMs
    └── shutdown.sh          # deallocate VMs (stop compute billing)
```

## Quick start

```sh
cd testbed
cp .env.local.example .env.local      # fill in subscription, tenant, SSH key
./scripts/deploy.sh                    # ~3–5 min; prints `ssh azureuser@<pip>` per node
```

Then bootstrap a stack and run the benchmark — see [`../samples/bench`](../samples/bench).

Tear down with `./scripts/teardown.sh` (keeps the LB IP / DNS) or
`./scripts/teardown.sh --purge-ip --yes` to release everything.

## Secrets

Nothing sensitive is committed. `parameters.bicepparam` and the scripts read
subscription/tenant/SSH-key/email from `testbed/.env.local`, which is
gitignored. `cloud-init.yaml.tpl` carries no secrets (Tailscale is gone), so
`deploy.sh` base64-encodes it verbatim.
