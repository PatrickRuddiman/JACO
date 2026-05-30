# ADR 0003: Orchestrator comparison benchmark

- **Status:** proposed
- **Date:** 2026-05-30
- **Issue:** #51

---


The issue is already a thorough spec; this comment commits the open questions, fixes the structure of the harness, and names the deliverable shape so implementation can start without further design.

## Resolved decisions

**Q1 — Limit-enforced vs unlimited.** **Run limited from day one.** PR #128 added `pull_policy` and PR #125 added the `deploy.resources` allow-list and forwarding; per-replica CPU/memory limits are honored today (the original #49 gap is closed for compose `deploy.resources` syntax). All four orchestrators get the same `cpus: 0.5, memory: 512m` per replica. Re-running unlimited later as a sensitivity sweep stays optional and out of scope for this issue.

**Q2 — Clean bed per orchestrator vs reprovision.** **Reprovision via `tests/testbed/scripts/deploy.sh`.** Faster than fighting cgroup/CNI/socket residue, cheaper than a debugging round, and the testbed bicep already supports it. Cost is small (3× B2s for the duration of one run × four orchestrators); time per run is dominated by the workload, not VM creation.

**Q3 — k8s flavor.** **Both kubeadm and k3s, and report them as distinct columns.** k3s already covers the "small k8s" use case; including kubeadm answers "what does the full thing cost on a B2s?" which is itself a finding. Document up front that kubeadm-on-B2s is cramped (2 vCPU / 4 GiB) and treat resource-contention readings on kubeadm as a feature, not a bias to control for.

Final column list: `JACO | Docker Swarm | k3s | kubeadm-k8s`.

**Q4 — Load generator location.** **A fourth VM in the same VNet** (`Standard_B2s`, same region, same subnet, no Tailscale path). Eliminates WAN noise and is the only fair control. Add the loadgen VM to `tests/testbed/template.bicep` behind a `loadgen: true` parameter so it doesn't burden ordinary E2E runs.

**Q5 — TLS in the bench.** **Internal CA / staging certs.** ACME against Let's Encrypt staging at the cadence a bench needs would burn through staging rate limits (and prod would be reckless). Stand up a minimal in-bed CA (a single CA cert mounted by each orchestrator's ingress) and pin the loadgen to trust it. Note in the methodology that real-ACME issuance time is captured separately and once per orchestrator under category A (install/control-plane), not on the data-plane hot path.

**Q6 — Stack ceiling.** **Stop at the expanded stack defined in the prerequisite** (web + api + redis + db + worker + broker, multi-network). A second profile (pure-CPU fan-out) gets a follow-up issue if the data justifies it; we don't speculate that work now.

## Harness shape

```
tests/bench/
  README.md                — methodology, decisions above, how to run
  Makefile                 — `make bench-all`, `make bench-<orch>`
  orchestrators/
    jaco/install.sh        — install + form cluster on jaco-1/2/3
    swarm/install.sh
    k3s/install.sh
    kubeadm/install.sh
  manifests/
    jaco/                  — copy of expanded tests/samples/jaco/
    swarm/                 — compose with native deploy.* (the keys JACO already honors too)
    k8s/                   — Deployments/Service/Ingress/StatefulSet, hand-corrected from kompose
  metrics/
    a_install.sh           — category A: install/control-plane footprint
    b_lifecycle.sh         — category B: deploy/update/scale/MTTR
    c_dataplane.sh         — category C: ingress throughput, latency, saturation, fairness
    d_network.sh           — category D: east-west latency/throughput, DNS hop, ACME timing
    e_efficiency.sh        — category E: overhead ratio, density
    f_resilience.sh        — category F: failover/quorum/self-heal
    g_operability.md       — category G: qualitative log + scoring rubric
    h_soak.sh              — category H: optional 4–24h
  loadgen/
    wrk.lua                — `GET /`, `GET /api/notes`, `POST /notes`
    vegeta.targets         — fixed-RPS plan
  collect.sh               — wraps a single category, dumps JSON per-orchestrator
  report/
    aggregate.py           — N-run median + spread → markdown tables
    template.md            — the final docs/benchmarks/orchestrator-comparison.md skeleton
  results/                 — raw outputs, gitignored except per-release archives
```

Each category script:
1. Asserts the bed is in a known state (`jaco status` / `kubectl get nodes` / `docker node ls`).
2. Runs the metric N=3 times, sleeping 30 s between runs (warm-cache controlled separately by a cold-run flag).
3. Writes `tests/bench/results/<orch>/<category>-<run>.json` with `{tool, version, command, raw_output, parsed_metrics}`.
4. Exits non-zero on environmental failure (cluster not ready, tool missing); never on a workload result.

`tests/bench/Makefile` orchestrates: `make bench-jaco` runs `deploy.sh` → `orchestrators/jaco/install.sh` → every metric script → `aggregate.py` → archive results. `make bench-all` does the same for all four columns in sequence (parallel runs would contaminate each other's bed).

## Prerequisite: expanded stack

The prerequisite from the issue body lands as a separate PR before the harness starts. Target shape:

- `web` (nginx) — unchanged
- `api` (Node/TS) — gains a read-through redis cache on `GET /notes`, falls back to postgres on miss
- `redis` — new, with persistence (named volume `redisdata`) so it counts as a stateful tier
- `db` (postgres-16) — unchanged
- `worker` (Node/TS) — consumes a `jobs` queue (redis list); non-ingress, scales independently
- `broker` — **redis doubles as broker** for v1 (kept simple); a separate NATS service is a follow-up if the data shows it
- networks: `frontend` (web↔api), `backend` (api↔{redis,db}), `jobs` (api↔worker via redis). Three networks so east-west isolation is actually exercised.

Acceptance for the prerequisite: `docker compose up`, `jaco apply`, and `kubectl apply -k k8s/` all bring the same logical stack up; loadgen scripts pass against each.

## Methodology section (must land in the report)

The `docs/benchmarks/orchestrator-comparison.md` template MUST include:

- **Bed spec** — verbatim from `tests/testbed/parameters.bicepparam`, with bicep commit sha
- **Orchestrator versions** — pinned, with install-command snapshots
- **Isolation between runs** — "reprovision via `deploy.sh` between orchestrators; sleep 30 s between same-orchestrator runs"
- **Cold vs warm** — category B includes both; categories C/D/E warm only; category A is cold by definition
- **Same-stack deviations** — table of per-orchestrator manifest differences (e.g. k8s StatefulSet vs compose volume, ingress controller choice)
- **Tools + versions** appendix — `wrk 4.2.0`, `vegeta 12.x`, `iperf3 3.16`, etc.
- **Decision log** — the six resolutions above, recorded so a future reader knows what was deliberate
- **Findings** — per-category narrative and a prioritized list of follow-up issues; this is the *output* of the benchmark, not its input

## Sequencing

Three PRs, in order:

1. **Stack expansion PR** — `tests/samples/jaco/` grows to the topology above; runs unmodified under `docker compose up` and `jaco apply` on the bed. Bench-blocked acceptance: a follow-up E2E proves multi-network DNS and per-network isolation are intact.
2. **Harness PR** — `tests/bench/` scaffolding, the four `orchestrators/*/install.sh` scripts, manifests, metric scripts that produce valid JSON, `aggregate.py`. Acceptance: `make bench-jaco` runs end-to-end on the bed and produces a populated `docs/benchmarks/orchestrator-comparison.md` with the JACO column filled, the other three columns empty.
3. **Baseline runs PR** — execute the harness against Swarm, k3s, kubeadm; populate the report; file follow-up issues for the gaps; commit raw results under `tests/bench/results/<release>/`.

## Acceptance (reaffirmed from issue body, with the decisions above folded in)

- [ ] Prerequisite stack expanded under `tests/samples/jaco/` and runs unmodified under all four targets.
- [ ] `tests/bench/` harness lands, with the structure above, and is documented.
- [ ] Translated manifests for Swarm, k3s, kubeadm checked in.
- [ ] `docs/benchmarks/orchestrator-comparison.md` populated for categories A–F (G qualitative; H optional), N=3, with the methodology section above.
- [ ] Findings section links to new follow-up issues.
- [ ] Loadgen runs from an in-VNet B2s, internal CA terminates TLS, all four orchestrators run limit-enforced (`cpus: 0.5, memory: 512m`).
- [ ] Reprovisioning between orchestrators is automated by the Makefile, not manual.

## Out of scope (reaffirmed)

- Resizing the bed; tuning each orchestrator beyond defaults; multi-cluster / federation; GPU scheduling; HPA / cluster-autoscaler.
