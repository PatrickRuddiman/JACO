# Benchmark rubric

How the four orchestrators (JACO, Kubernetes, k3s, Docker Swarm) are graded
when running the **identical** [workload](../workload) on the **identical**
[testbed](../../testbed). Only the orchestration layer changes between runs.

The methodology behind these choices — and the literature it draws on — is in
[`docs/benchmarking-methodology.md`](docs/benchmarking-methodology.md).

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
5. **Repeat and report variance.** `run.sh --repeat N` takes N measured samples
   and records each one plus mean / stdev / **95% CI** (`load.stats`). A single
   run can't establish a tail percentile; the scorecard prints rps ±95% CI, and
   **overlapping intervals mean a statistical tie**, not a winner.
6. **Warm-up, then measure.** A discarded warm-up load (`BENCH_WARMUP_DURATION`,
   default 20s) precedes the measured samples so caches/JITs are at steady state
   — cold-start effects don't leak into steady-state numbers.
7. **Watch the load generator.** The generator host's CPU is sampled around each
   run (`load_generator.cpu_pct_max`, plus a `saturated` flag at >80%). A
   saturated generator caps offered load and would be mistaken for the stack's
   ceiling — the scorecard flags it so such a run isn't trusted.
8. **Account for control-plane cost.** Idle node memory + load average are
   snapshotted post-deploy/pre-load (`overhead.*`); with an identical idle
   workload the cross-stack delta approximates each control plane's footprint.

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

The throughput/latency values fed into scoring are the **per-sample means**
(`--repeat N`); the scorecard also prints the rps 95% CI so ties are visible.
**Control-plane overhead and load-generator CPU are reported raw (informational),
not yet folded into the composite** — the idle-memory proxy is deliberately coarse
(it isn't per-process attribution), so promoting it to a scored dimension waits on
a more precise collector.

The **composite** is the weighted sum (weights above). `scorecard.sh` computes
this from `results/*/result.json` and prints a sorted table. With a single
stack's results present, every normalized score is 100 — the comparison is only
meaningful once ≥2 stacks have run.

## Running

```sh
# from tests/samples/bench, with the testbed deployed and node IPs resolvable
./run.sh jaco --repeat 5       # deploy + warm-up + 5 measured samples + record
./collect.sh                   # snapshot node resource stats + bundle for download
# ...repeat ./run.sh for k8s / k3s / swarm...
./scorecard.sh                 # cross-stack comparison table (rps mean ±95% CI)
```

Useful env: `BENCH_REPEAT` (default 1; or `--repeat N`), `BENCH_WARMUP_DURATION`
(default `20s`, `0` to disable), `BENCH_VUS`, `BENCH_DURATION`, `BENCH_TARGET`,
`BENCH_HOST_HEADER`. Each run writes `results/<stack>-<timestamp>/result.json`
(per-sample `samples[]` + `stats`, `load_generator`, `overhead`) plus the raw k6
summaries, app metrics, and node stats. `results/` is gitignored.

## Caveats

- **Ingress terminators differ per stack** (JACO/Swarm use Caddy, k3s uses
  Traefik, kubeadm uses ingress-nginx) — all over HTTPS via ACME staging, but a
  small ingress-attributable latency delta can't be fully excluded. For a pure
  orchestration comparison, set `BENCH_TARGET=http://<lb-ip>` +
  `BENCH_HOST_HEADER=<domain>` (HTTP, no terminator) and keep it constant across
  all stacks in the comparison.
- **Don't over-read small gaps.** Compare rps with its 95% CI: overlapping
  intervals are a tie. The LOC proxy for ease-of-setup is rough by design; treat
  the composite as a decision aid, not a verdict. Raw columns are the truth.
- **One bed, one stack at a time, randomized order.** Run stacks sequentially on
  the same bed (or re-provision between) so they never contend for nodes, and
  randomize which stack goes first across repetitions to avoid order effects.
- TTL and bootstrap time can be skewed by image-layer cache reuse; for a clean
  time-to-ready, ensure symmetric cold/warm image-cache state across stacks.
