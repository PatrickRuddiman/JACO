# Smoke test

How to live-smoke a change against **real** JACO infrastructure on the Azure
testbed. JACO is Linux-only (WireGuard, nftables, CAP_NET_ADMIN), so it cannot
run on a Windows/macOS dev box — the binaries are cross-compiled and run on real
Debian VMs provisioned by [`tests/testbed`](tests/testbed/README.md).

This file is authoritative for `/smoke-test`. Fall back to the generic phases
only for what it doesn't cover.

## Boot infra

Provision via the testbed bicep into resource group `JACO` (region
`centralus`). Subscription + tenant come from `tests/testbed/.env.local`
(gitignored — `AZ_SUBSCRIPTION` / `AZ_TENANT`); never commit those IDs.

- **1 node** is enough for any control-plane change (registry creds, tokens,
  apply, audit) — a single `cluster init` makes the node the raft leader, which
  is all the write path needs.
- **3 nodes** only when the change must prove **raft replication across nodes**
  (followers serving replicated state, membership, mesh).

Lightest path (single node), run from the repo root in PowerShell:

```powershell
# 1. cross-compile the real binaries from your branch (no CGO; Linux target)
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -trimpath -ldflags "-s -w" -o .\dist\smoke\jacod .\cmd\jacod
go build -trimpath -ldflags "-s -w" -o .\dist\smoke\jaco  .\cmd\jaco

# 2. deploy ONE VM (incremental; override vmCount). deploy.sh needs
#    tests/testbed/.env.local with AZ_SUBSCRIPTION/AZ_TENANT (gitignored).
cd tests\testbed
$env:ADMIN_PUBLIC_KEY = (Get-Content .\.ssh\jaco.pub -Raw).Trim()   # ssh-keygen -t ed25519 -f .ssh\jaco first
$env:CUSTOM_DATA = [Convert]::ToBase64String([IO.File]::ReadAllBytes("$PWD\cloud-init.yaml.tpl"))
az group create -n JACO -l centralus -o none
az deployment group create -g JACO -n smoke --parameters parameters.bicepparam --parameters vmCount=1 -o table
az vm list-ip-addresses -g JACO -o table   # grab public + private IPs
```

SSH key: `tests/testbed/.ssh/jaco`, user `azureuser`. Wait for
`/var/lib/cloud/instance/testbed-base-done` before copying.

## Run jacod + init

jacod boots fine **without docker** (it logs `docker unreachable, runtime
disabled` and keeps the control plane up) and a single-node leader needs **no
WireGuard mesh**, so the control-plane path needs no extra packages.

Copy `jacod`, `jaco`, and a config to the VM, then (as root):

```bash
# /etc/jacod.yaml — pin addrs to the node's PRIVATE IP, disable ACME
cat >/etc/jacod.yaml <<'EOF'
data_dir: /var/lib/jaco
listen_addr: <PRIVATE_IP>:7000
cluster_addr: <PRIVATE_IP>:7001
unix_socket: /run/jaco/jaco.sock
wg_port: 51820
log_level: debug
ipam_pool: 10.244.0.0/16
acme_enabled: false
EOF
sudo mkdir -p /var/lib/jaco /run/jaco
sudo bash -c 'nohup ./jacod --config /etc/jacod.yaml >/var/log/jacod.log 2>&1 &'
sudo ./jaco cluster init --socket /run/jaco/jaco.sock --name smoke   # prints operator_token (save it)
```

The CLI talks to the local daemon over `--socket /run/jaco/jaco.sock` (the
socket is `root`-owned, so `sudo`). Off-node/remote calls instead use
`--server <priv-ip>:7000 --token <operator_token>` (TLS; default CA at
`/var/lib/jaco/node/ca.crt`).

## Add nodes (only for replication tests)

Deploy with `--parameters vmCount=3`, boot jacod on each new node with its own
private IP in the config, then for **each** joiner (token is single-use):

```bash
# on the leader:
sudo ./jaco node issue-join-token --socket /run/jaco/jaco.sock
# on the joiner:
sudo ./jaco node join --socket /run/jaco/jaco.sock --peer <LEADER_PRIV_IP>:7000 --token <JOIN_TOKEN>
# verify (from leader, TLS):
sudo ./jaco node list --server <LEADER_PRIV_IP>:7000 --token <operator_token>   # expect all NODE_STATUS_READY
```

## Signals to read

Never trust exit 0 alone. For a control-plane change, read **all** of:

- **CLI response** — the command's own output reflects the intended state, e.g.
  `jaco registry list` / `jaco token list` / `jaco status`.
- **Audit log** — `jaco audit --socket /run/jaco/jaco.sock` shows the expected
  event type(s); confirm **no secret material** is recorded.
- **Daemon log** — `/var/log/jacod.log`: zero `level=ERROR` / `level=WARN` on
  your path, and no secret/plaintext leaked.
- **Replication (3-node)** — write on the leader, then read the **follower's**
  local socket and confirm it serves the same state.

## Worked example — per-namespace registry credentials

The change keys credentials by `host[:port][/namespace]` (longest-prefix at
pull time) instead of collapsing every `ghcr.io/<org>` to bare `ghcr.io`.

```bash
S=/run/jaco/jaco.sock
echo secretA | sudo ./jaco registry login ghcr.io/org-a -u userA --password-stdin --socket $S
echo secretB | sudo ./jaco registry login ghcr.io/org-b -u userB --password-stdin --socket $S
echo secretF | sudo ./jaco registry login ghcr.io       -u fallback --password-stdin --socket $S
sudo ./jaco registry list --socket $S          # EXPECT 3 distinct rows (pre-fix: 1 collapsed row)
sudo ./jaco audit --socket $S | grep registry  # EXPECT 3 registry_credential_upsert, namespaced keys, NO secret
sudo ./jaco registry logout ghcr.io/org-a --socket $S
sudo ./jaco registry list --socket $S          # EXPECT only that key gone; org-b + bare ghcr.io remain
```

Canonicalization runs in the **FSM `Apply`** (deterministic on every node), so
on a 3-node cluster a leader write replicates byte-identically to followers —
verify by reading `registry list` from a follower's socket.

Pull-time longest-prefix resolution needs a real private image pull (docker + a
private registry); it's covered by unit tests
(`internal/runtime/pull/auth_test.go`) and is the one surface this flow does not
observe live.

## Teardown

```powershell
az group delete --name JACO --yes        # destroys all VMs/disks/NICs/PIPs/NSG/VNet/LB
```

`tests/testbed/scripts/teardown.sh --yes` does the same and also drops the
ephemeral SSH key. Both **preserve** the persistent LB public IP `jaco-lb-pip`
in the separate `jaco-net` RG (the `jaco.sh` A record stays valid). Pass
`--purge-ip` to teardown.sh only if you really want that gone too. Remove local
artifacts: `dist/smoke`, `tests/testbed/.ssh/jaco*`, `tests/testbed/.env.local`.
