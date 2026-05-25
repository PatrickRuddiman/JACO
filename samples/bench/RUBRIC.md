# Benchmark rubric

How the four orchestrators (JACO, Kubernetes, k3s, Docker Swarm) are graded
when running the **identical** [workload](../workload) on the **identical**
[testbed](../../testbed). Only the orchestration layer changes between runs.

## Fairness principles (how bias is eliminated)

1. **One workload, one set of images.** Same `web` / `api` / `redis` images and
   the same `replicas` and resource limits on every stack (`cpus`/`memory` caps
   in each manifest). No per-stack app tuning.
2. **One neutral substrate.** Every stack bootstraps from the same minimal
   Debian VMs (no container runtime or orchestrator pre-installed), so install
   cost counts fairly toward each stack's score.
3. **One measurement path.** Throughput and latency come from the **same k6
   scenario** (`loadgen/scenario.js`) hitting the **same ingress** with the same
   VUs/duration/read-write mix. The generator is `constant-vus`, so a slow stack
   shows up as worse latency, never as reduced offered load.
4. **Automated end-to-end.** `run.sh <stack>` deploys, measures, and records
   without manual steps in the measured window. The only human inputs are the
   subjective ease-of-setup notes (below), kept separate from the measured score.

## Dimensions

| # | Dimension          | Source                              | Direction | Weight |
|---|--------------------|-------------------------------------|-----------|--------|
| 1 | Ease of setup      | `setup_loc` (bootstrap LOC proxy) + operator notes | lower better | 0.20 |
| 2 | Time-to-ready (TTL)| harness: deploy → first 2xx at ingress | lower better | 0.20 |
| 3 | Throughput         | k6 `http_reqs` rate (req/s)         | higher better | 0.25 |
| 4 | Latency            | k6 `http_req_duration` p95 (ms)     | lower better | 0.20 |
| 5 | Reliability        | `1 − http_req_failed` rate          | higher better | 0.10 |
| 6 | Replication        | app `bench_replica_lag_seconds`     | lower better | 0.05 |

**Ease of setup** has an automated proxy (total lines in `samples/<stack>/bootstrap`)
so it is reproducible, plus a place for an operator's qualitative 1–5 (clarity,
number of moving parts, footguns). The automated proxy is what feeds the score;
the qualitative note is recorded alongside for context.

**Time-to-ready** is wall-clock from the deploy command to the first successful
ingress response — it folds in image pulls, scheduling, mesh/CNI convergence,
and ingress/cert readiness, which is exactly the operator-visible "how long
until it serves traffic."

**In-cluster performance** (throughput, latency, reliability) is reported by the
stack itself: the `api` exposes a Prometheus `/metrics` endpoint (HTTP latency,
per-op Redis latency, replication lag), and `collect.sh` snapshots `docker stats`
per node. The authoritative throughput/latency numbers are k6's client-side
measurement at the ingress, because that one path is identical for every stack.

## Scoring

Each dimension is normalized across the stacks that have results, so the best
stack on a dimension scores **100** and the others scale relative to it:

- higher-better: `score = value / max(values) × 100`
- lower-better:  `score = min(positive values) / value × 100` (0 ⇒ 100)
- reliability:   `score = (1 − error_rate) × 100` (absolute)

The **composite** is the weighted sum (weights above). `scorecard.sh` computes
this from `results/*/result.json` and prints a sorted table. With a single
stack's results present, every normalized score is 100 — the comparison is only
meaningful once ≥2 stacks have run.

## Running

```sh
# from samples/bench, with the testbed deployed and node IPs resolvable
./run.sh jaco                  # deploy + measure + record one stack
./collect.sh                   # snapshot node resource stats + bundle for download
# ...repeat ./run.sh for k8s / k3s / swarm (adapters land in the follow-up)...
./scorecard.sh                 # cross-stack comparison table
```

Each run writes `results/<stack>-<timestamp>/result.json` plus the raw k6
summary, app metrics, and node stats. `results/` is gitignored.

## Caveats

- TLS termination differs per stack; for an apples-to-apples HTTP comparison set
  `BENCH_TARGET=http://<lb-ip>` + `BENCH_HOST_HEADER=<domain>` and keep it
  constant across all stacks in a comparison.
- The LOC proxy for ease-of-setup is rough by design; treat the composite as a
  decision aid, not a verdict. The raw columns in the scorecard are the truth.
- Run stacks one at a time on the shared bed (tear the workload down between
  runs) so they never contend for the same nodes.
