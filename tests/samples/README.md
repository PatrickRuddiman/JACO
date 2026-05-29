# samples — comparative orchestrator benchmark

A reproducible, **bias-controlled** comparison of JACO against Kubernetes
(kubeadm), k3s, and Docker Swarm: deploy one identical workload on each
orchestrator, on the same hardware, and grade them with the same rubric.

```
samples/
├── workload/     # the one app deployed everywhere (web + api + redis primary/replicas)
├── jaco/         # JACO deployment + 3-node bootstrap          ← implemented
├── k8s/          # Kubernetes (kubeadm) bootstrap + manifests   ← implemented
├── k3s/          # k3s bootstrap + manifests                    ← implemented
├── swarm/        # Docker Swarm bootstrap + stack               ← implemented
└── bench/        # grading harness: load gen, metrics, rubric, scorecard
```

The Azure provisioning that creates the 3-node Debian substrate lives in the
sibling [`../testbed`](../testbed) directory.

## The idea

Every stack runs the **same** [`workload`](workload): an nginx `web` tier that
proxies `/api/*` to a Node `api` tier, which writes to a **Redis primary** and
reads from **Redis replicas**. Same images, same replica counts, same CPU/memory
limits. Only the orchestration layer differs between runs — so any difference in
the numbers is attributable to the orchestrator, not the app.

The [`bench`](bench) harness deploys a stack, measures **time-to-ready**, drives
a fixed [k6 load scenario](bench/loadgen/scenario.js) at the ingress, collects
the metrics the stack reports about itself, and scores each stack against the
[rubric](bench/RUBRIC.md): ease of setup, TTL, throughput, latency, reliability,
and replication lag.

## Quick start (JACO)

```sh
# run from the repo root.
# 1. provision the substrate (see ../testbed/README.md)
( cd tests/testbed && cp .env.local.example .env.local && ./scripts/deploy.sh )

# 2. bootstrap + benchmark JACO
cd tests/samples/bench
export BENCH_PUBLIC_IPS="<n1> <n2> <n3>" BENCH_PRIVATE_IPS="<p1> <p2> <p3>"
./run.sh jaco
./collect.sh
./scorecard.sh
```

## Status

| stack | bootstrap | manifests | bench adapter |
|-------|-----------|-----------|---------------|
| JACO  | ✅        | ✅        | ✅            |
| swarm | ✅        | ✅        | ✅            |
| k3s   | ✅        | ✅        | ✅            |
| k8s   | ✅        | ✅        | ✅            |

All four stacks are implemented and bed-validated against the shared workload,
testbed, and grading harness. The remaining #51 work is a single clean **4-way
scorecard run** — all stacks deployed fresh, back-to-back, on one bed — for the
authoritative comparison (the per-stack `result.json`s to date come from separate
bed runs, so cross-stack TTL/composite aren't yet apples-to-apples).
