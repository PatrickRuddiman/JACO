# samples — comparative orchestrator benchmark

A reproducible, **bias-controlled** comparison of JACO against Kubernetes
(kubeadm), k3s, and Docker Swarm: deploy one identical workload on each
orchestrator, on the same hardware, and grade them with the same rubric.

```
samples/
├── workload/     # the one app deployed everywhere (web + api + redis primary/replicas)
├── jaco/         # JACO deployment + 3-node bootstrap          ← implemented
├── k8s/          # Kubernetes (kubeadm) bootstrap + manifests   ← follow-up (stub)
├── k3s/          # k3s bootstrap + manifests                    ← follow-up (stub)
├── swarm/        # Docker Swarm bootstrap + stack               ← follow-up (stub)
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
# 1. provision the substrate (see ../testbed/README.md)
cd ../testbed && cp .env.local.example .env.local && ./scripts/deploy.sh

# 2. bootstrap + benchmark JACO
cd ../samples/bench
export BENCH_PUBLIC_IPS="<n1> <n2> <n3>" BENCH_PRIVATE_IPS="<p1> <p2> <p3>"
./run.sh jaco
./collect.sh
./scorecard.sh
```

## Status

| stack | bootstrap | manifests | bench adapter |
|-------|-----------|-----------|---------------|
| JACO  | ✅        | ✅        | ✅            |
| k8s   | stub      | stub      | stub          |
| k3s   | stub      | stub      | stub          |
| swarm | stub      | stub      | stub          |

The workload, testbed, and grading harness are complete and validated against
JACO. The other three stacks reuse all of that unchanged — only their bootstrap
+ manifests + a thin bench adapter remain (each stack's README specifies them).
