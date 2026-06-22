# Smoke test

How to live-smoke a change against **real** JACO infrastructure on the Azure
testbed. JACO is Linux-only (WireGuard, nftables, CAP_NET_ADMIN), so it cannot
run on a Windows/macOS dev box — the daemon is packaged as a `.deb` and run on
real Debian VMs provisioned by [`tests/testbed`](tests/testbed/README.md).

This file is authoritative for `/smoke-test`. Fall back to the generic phases
only for what it doesn't cover.

## The bed is non-negotiable

A JACO smoke runs on the **full three-node bed with Docker, and it is not a pass
until a real stack is deployed with `jaco apply` and observed RUNNING across the
cluster.** "It compiled", "the unit test is green", "one node answered" prove
nothing about an orchestrator — the entire point is multi-node scheduling, the
WireGuard mesh, raft replication, image pulls, and ingress. So every smoke stands
up all of:

- **3 Debian nodes** (`jaco-1..3`), each running `jacod` under systemd from the
  real `.deb` (never `nohup ./jacod`), joined into one raft cluster.
- **Docker on every node** — the `.deb` Depends on it and `jacod` dials
  `/var/run/docker.sock`; with no runtime, nothing actually runs.
- **An in-cluster `registry:2`** on node-1 holding the workload images (the
  operator host can't reach the private registry — only 22/80/443 are public).
- **A real deployed stack** — the [`tests/samples/jaco`](tests/samples/jaco)
  `bench` workload (redis primary + replicas, a cross-node Postgres
  primary/replica pair, an api tier, a web tier behind one ingress route),
  applied with `jaco apply` and reachable end-to-end over the ingress LB.

Even a control-plane-only change (tokens, audit, registry creds) runs on this
same live bed: a follower has to be present to prove the write replicated, and
the runtime has to be present to prove you didn't break a pull or a reconcile.

## Boot infra

Provision the testbed bicep into resource group `JACO` (region `centralus`).
Subscription + tenant come from `tests/testbed/.env.local` (gitignored —
`AZ_SUBSCRIPTION` / `AZ_TENANT`); never commit those IDs. The bed is **3 nodes**
(`parameters.bicepparam` defaults `vmCount=3`); each gets its own public IP for
SSH, and a persistent Standard LB fronts 80/443 across all three at one stable
address (`jaco-lb-pip` in the separate `jaco-net` RG — point the wildcard
`*.jaco.prcs.xyz` (and apex `jaco.prcs.xyz`) A record at it once).

From the repo root in PowerShell:

```powershell
cd tests\testbed
$env:ADMIN_PUBLIC_KEY = (Get-Content .\.ssh\jaco.pub -Raw).Trim()   # ssh-keygen -t ed25519 -f .ssh\jaco first
$env:CUSTOM_DATA = [Convert]::ToBase64String([IO.File]::ReadAllBytes("$PWD\cloud-init.yaml.tpl"))
az group create -n JACO -l centralus -o none
az deployment group create -g JACO -n smoke --parameters parameters.bicepparam -o table
az vm list-ip-addresses -g JACO -o table   # grab the per-node public + private IPs (node-1 first)
```

SSH key: `tests/testbed/.ssh/jaco`, user `azureuser`. The base cloud-init installs
**no** container runtime and **no** orchestrator — Docker and jacod are installed
by the bootstrap below. Wait for `/var/lib/cloud/instance/testbed-base-done` on
each node before bootstrapping.

## Stand up the cluster + deploy the stack

Use the repo's one-shot — it is the authoritative bring-up and does the whole bed
in order: installs Docker + the `.deb` on all three nodes, stands up the
registry, **builds and pushes the workload images on node-1**, forms the cluster
(`cluster init` on node-1, `node join` on 2 and 3), runs `systemctl enable jaco`
after each init/join (the package ships the unit *disabled* by design — the
cluster-commit is the right enable signal, #151), and finally `jaco apply`s the
stack:

```bash
# from an operator host with bash + az + the per-bed SSH key:
export RESOURCE_GROUP=JACO VM_NAME_PREFIX=jaco      # resolves the 3 node IPs via az
# (or skip az: export BENCH_PUBLIC_IPS="<n1> <n2> <n3>" BENCH_PRIVATE_IPS="<n1p> <n2p> <n3p>")
tests/samples/jaco/bootstrap/bootstrap.sh           # DEB=path to skip the `make package` build
```

What lands on each node (`tests/samples/jaco/bootstrap/install-node.sh`): Docker
from get.docker.com, an `insecure-registries` entry for the node-1 registry, the
`jaco_*.deb` (whose postinstall creates the `jaco` service user **in the docker
group** so jacod reaches the socket unrooted), `acme_ca` pinned to Let's Encrypt
**staging** (throwaway bed — staging certs aren't browser-trusted, so verify with
`curl -k`; override `ACME_CA=` for prod), and the daemon started under systemd,
uninitialized.

The CLI talks to the local daemon over `/run/jaco/jaco.sock` (root-owned → `sudo`).
`docker` on a node also needs `sudo` (only the `jaco` service user is in the
docker group, not `azureuser`). Off-node calls instead use `--server
<priv-ip>:7000 --token <operator_token>` (TLS; default CA at
`/var/lib/jaco/node/ca.crt`). Mesh traffic — gRPC `:7000`, raft `:7001`,
WireGuard `:51820` — stays on the private VNet (`172.16.0.0/24`); only the
ingress (80/443) is public, via the LB.

Doing the cluster by hand instead of the one-shot? The per-node join handshake
(token is single-use) is:

```bash
# on node-1 (leader):
sudo jaco cluster init
sudo jaco node issue-join-token            # prints: jaco node join --peer=… --token=…
# on each joiner:
sudo jaco node join --peer=<NODE1_PRIV>:7000 --token=<JOIN_TOKEN>
# on leader AND every joiner — persist the unit across reboots:
sudo systemctl enable jaco
# verify from node-1:
sudo jaco node list                        # expect all three NODE_STATUS_READY
```

## Exercise the change

Drive your changed code path through the nearest entrypoint **on the running
bed** — `jaco apply` a manifest that forces the new branch, a `jaco registry` /
`jaco token` / `jaco get` command, an ingress request — exactly as a real
operator would. A generic happy-path call that dodges your change tests nothing;
craft input that *forces* the new/changed code to run.

## Signals to read

Never trust exit 0 alone. Collect from **every** surface that applies and make
them agree — one green surface is not a pass:

- **CLI response** — the command reflects the intended state (`jaco status`,
  `jaco get deployment|replicas|route`, `jaco cluster status`, `jaco node list`).
- **Docker runtime, per node** — `sudo docker ps` on **each** of the three nodes
  shows the jaco-managed containers actually **running**, and the replicas are
  **distributed across nodes** (not all stacked on the leader). `jaco get
  replicas` must agree: every replica RUNNING, spread across hostnames, and the
  pinned `pg-primary` / `pg-replica` landing on `jaco-2` / `jaco-3` respectively.
- **Ingress, end-to-end** — through the LB public IP (the `jaco.prcs.xyz` A record) the
  deployed app actually answers:
  ```bash
  curl -sk https://jaco.prcs.xyz/                 # web HTML (or LB IP + -H 'Host: jaco.prcs.xyz')
  curl -sk https://jaco.prcs.xyz/api/notes        # JSON list (api reads a redis replica)
  curl -sk -XPOST https://jaco.prcs.xyz/api/notes -H 'content-type: application/json' -d '{"text":"hi"}'
  curl -sk https://jaco.prcs.xyz/api/metrics      # Prometheus metrics from an api replica
  ```
  `jaco get route jaco.prcs.xyz` shows **READY n/n non-zero** — real running upstreams.
  `0/0` means no backend is up (the silent-503 indicator) and is a **fail**.
- **Audit log** — `jaco audit` shows the expected event type(s) and records **no
  secret material**.
- **Daemon log** — `journalctl -u jaco` on the node: zero `level=ERROR` /
  `level=WARN` on your path, nothing leaked, and — because Docker is present —
  **no `docker unreachable, runtime disabled`** line (its presence here is itself
  a regression).
- **Replication (3-node)** — write on the leader, then read a **follower's** local
  socket (`sudo jaco get … ` / `sudo jaco registry list` on node-2 or node-3) and
  confirm it serves byte-identical replicated state. The FSM `Apply` is
  deterministic on every node, so a leader write must land on followers verbatim.

### Multi-node TLS (cross-host cert propagation)

The LB fronts `:443` on all three nodes, but a `tls: auto` cert is obtained by
the raft **leader** only; followers must serve the *same* leaf from replicated
storage. Two behaviours matter here, both validated live on the bed:

- **Follower self-heal.** When the leader's prod leaf replicates, a follower's
  re-rendered Caddy config is byte-identical, so the debounced reloader would
  skip `caddy.Load` and the follower would serve no TLS until a restart. `jacod`
  drives a **level-triggered** forced reload instead: each reconcile tick, any
  `tls: auto` domain whose **complete** prod resource (`.crt` *and* `.key`,
  non-staging) has replicated but whose leaf the follower isn't yet serving gets
  one forced `caddy.Load`; the domain is latched only once the follower's cert
  cache actually serves that exact leaf, so a reload that loses the race with
  replication is retried, never abandoned (and the completeness gate keeps
  certmagic on the load path — a follower never starts ACME). Follower log
  marker: `stagefirst follower force-reloading to load replicated prod cert`,
  which **stops** once the node serves the leaf.

- **Challenge distribution (cluster-wide, #189).** ACME validation is HTTP-01
  **only** — the rendered automation policy disables TLS-ALPN-01, whose key-auth
  is node-local and therefore unanswerable behind an L4 (TCP-passthrough) LB.
  CertMagic keys its distributed HTTP-01 tokens by **CA prefix**, so a node
  rendering a different CA than the order initiator used to 404 the validation
  request (`"distributed":false` and the wrong CA in the certmagic log). `jacod`
  now taps storage for every `/challenge_tokens/` write and republishes the
  key-auth through a **CA-agnostic, token-keyed** raft set, served by a
  `jaco_acme_challenge` handler prepended to `:80`. The result: **any** node
  answers **any** in-flight challenge, so Let's Encrypt's multi-perspective
  validation succeeds no matter which backend the LB fans it to — the prod cert
  issues behind the **full three-node pool with no LB-shrink workaround**. In the
  log the issuing node serves `"distributed":false` and the followers serve the
  *same* challenge `"distributed":true`. On repeated prod-issuance failure the
  stage-first controller now backs off exponentially (15m -> 30m -> 1h cap) and
  emits `CERTIFICATE_FAILED{acme_environment:prod}` instead of re-ordering every
  ~5 min — the old cadence that self-sustained LE's failed-authorization limit.

Validate the whole path with a **fresh promotion** under the wildcard DNS, with the
**full LB pool intact** (all three NICs), so the cert appears while the followers
are already running the build under test — a node restart would mask the
distribution + self-heal by loading the cert on the normal startup path. Reproducing
#189 requires **prod** ACME (a non-prod `acme_ca` skips the stage-first -> prod
promotion), so bring the bed up with
`ACME_CA=https://acme-v02.api.letsencrypt.org/directory`:

```bash
DOMAIN=jaco.prcs.xyz   # the bench route already exercises this; add another *.jaco.prcs.xyz
                       # route + `sudo jaco apply` on the leader for an extra fresh order
# 1. watch the order: staging then prod (issuer acme-v02), http-01 only (no tls-alpn),
#    and challenges fanning across nodes — initiator distributed:false, a follower
#    distributed:true (run on each node; the LB picks which nodes the CA perspectives hit):
sudo journalctl -u jaco.service --no-pager \
  | grep -E 'challenge|served key authentication|certificate|obtain' | tail -20
# 2. per-node + LB proof: identical prod leaf (same serial) under public trust:
for ip in <n1> <n2> <n3> <lb>; do echo | openssl s_client -connect $ip:443 \
  -servername "$DOMAIN" 2>/dev/null | openssl x509 -noout -serial -issuer; done
# 3. LB round-robin trust (full pool) — same serial every hit, chain publicly trusted:
for i in $(seq 1 10); do curl -s -o /dev/null \
  -w 'http=%{http_code} ssl_verify=%{ssl_verify_result}\n' "https://$DOMAIN/"; done
# 4. audit records both promotions:
sudo jaco audit | grep -E 'certificate_issued|certificate_failed'
```

`ssl_verify=0` on a plain `curl` (no `-k`) proves the prod chain is publicly
trusted; **identical serials across all three nodes and the LB** prove every
follower serves the leader's replicated leaf, not a self-signed fallback, and that
the LB never lands on a node without the cert. Expect `certificate_issued` for
**both** `acme_environment=staging` and `acme_environment=prod`. If the order never
completes (the initiator keeps logging `distributed:false` while the CA reports
404s from other perspectives), that's a challenge-distribution regression of #189;
if a follower serves a stale/self-signed leaf, that's the cross-host propagation
regression of #188.

Verified live on the Azure testbed (3 nodes behind the Standard L4 LB, prod LE,
`jaco.prcs.xyz`): the order completed first try with the **full** backend pool —
`http-01` only (zero `tls-alpn`), the initiator served `distributed:false` and a
follower served the same challenge `distributed:true` across Let's Encrypt's
multi-perspective source IPs; all three nodes **and** the LB served one identical
prod leaf (`issuer=Let's Encrypt`, 10/10 LB handshakes the same serial); and
`jaco audit` recorded `certificate_issued` for both `staging` and `prod`.

## Worked example — per-namespace registry credentials

The change keys credentials by `host[:port][/namespace]` (longest-prefix at
pull time) instead of collapsing every `ghcr.io/<org>` to bare `ghcr.io`.

```bash
echo secretA | sudo jaco registry login ghcr.io/org-a -u userA --password-stdin
echo secretB | sudo jaco registry login ghcr.io/org-b -u userB --password-stdin
echo secretF | sudo jaco registry login ghcr.io       -u fallback --password-stdin
sudo jaco registry list           # EXPECT 3 distinct rows (pre-fix: 1 collapsed row)
sudo jaco audit | grep registry   # EXPECT 3 registry_credential_upsert, namespaced keys, NO secret
sudo jaco registry logout ghcr.io/org-a
sudo jaco registry list           # EXPECT only that key gone; org-b + bare ghcr.io remain
```

Canonicalization runs in the **FSM `Apply`** (deterministic on every node), so on
this 3-node cluster a leader write replicates byte-identically to followers —
verify by running `sudo jaco registry list` against a **follower's** socket and
confirming the same three rows.

Pull-time longest-prefix resolution is proven by deploying images from the
namespaces above on the live bed (containers reach RUNNING with the right
credential) and is also covered by unit tests
(`internal/runtime/pull/auth_test.go`).

### Regression guard — single namespace credential must cover its whole host (#172)

Keying credentials per namespace (above) silently broke a single-credential
deployment: before #171 a `registry login HOST/ns` login was stored under the
bare host key and authenticated *every* path on that host; #171 began
preserving the namespace, so an image on a **sibling** path (`HOST/other/app`)
matched no key, pulled anonymously, hit a registry `401`, and — because
`pull.Pull` retries forever — left the replica stuck `PENDING` with an empty
`container_id` and no docker events (the deploy "rolls indefinitely" when a
`depends_on` dependent is gated on it).

The fix (`pull.ResolveCredentialKey`) restores host-wide coverage **only when a
host has exactly one credential**; multiple namespace-scoped credentials on one
host still resolve independently (an unconfigured sibling stays anonymous).

Reproduce on the bed with a token-auth registry whose ACL scopes each namespace
separately (a single shared htpasswd would mask the bug — any valid credential
pulls any path). Register **one** namespace-scoped credential whose account is
authorized for several namespaces, then deploy stacks pulling from sibling
namespaces:

```bash
# robot is authorized (registry ACL) for team-a, team-b AND team-c:
echo secretR | sudo jaco registry login reg.example.com:5000/team-a -u robot --password-stdin
sudo jaco registry list            # EXPECT 1 row: reg.example.com:5000/team-a
# deploy three stacks: images team-a/app, team-b/app, team-c/app
# EXPECT (fixed): all three RUNNING (docker ps across nodes); jacod log has ZERO "anonymous token" / 401 lines
# Pre-fix: team-a RUNNING, team-b/team-c stuck PENDING with
#   "image pull failed ... failed to fetch anonymous token ... 401 Unauthorized"
```

Verified live on the Azure testbed: pre-fix the sibling-path stacks logged the
401 above and never started; post-fix all three pull via the sole-host-credential
fallback and reach RUNNING with no anonymous-token errors.

## Teardown

```powershell
az group delete --name JACO --yes        # destroys all VMs/disks/NICs/PIPs/NSG/VNet/LB
```

`tests/testbed/scripts/teardown.sh --yes` does the same and also drops the
ephemeral SSH key. Both **preserve** the persistent LB public IP `jaco-lb-pip`
in the separate `jaco-net` RG (the `jaco.prcs.xyz` A record stays valid). Pass
`--purge-ip` to teardown.sh only if you really want that gone too. Remove local
artifacts: `dist/package`, `dist/staging`, `dist/smoke`, `tests/testbed/.ssh/jaco*`,
`tests/testbed/.env.local`.
