# bench — the grading harness

Deploy the [shared workload](../workload) on each orchestrator, measure it the
same way every time, and produce a comparative scorecard. See
[`RUBRIC.md`](RUBRIC.md) for what's graded and how bias is controlled.

## Layout

```
bench/
├── RUBRIC.md          # dimensions, weights, fairness rules, scoring
├── docs/              # benchmarking-methodology.md — why we measure it this way
├── run.sh             # deploy + warm-up + N measured samples + record ONE stack
├── collect.sh         # snapshot node resource stats + bundle a run for download
├── scorecard.sh       # cross-stack comparison from results/*/result.json
├── lib/common.sh      # shared helpers (node resolution, ssh, ready-poll, cpu/overhead)
├── loadgen/scenario.js# the one k6 scenario every stack is measured with
├── adapters/          # per-stack deploy/collect/teardown (all four implemented)
│   ├── jaco.sh  k8s.sh  k3s.sh  swarm.sh
└── results/           # per-run output (gitignored)
```

## Operator prerequisites

- `curl`, `jq`, and either `docker` (to run `grafana/k6`) or a native `k6`.
- SSH access to the testbed nodes. `SSH_KEY` defaults to the per-bed key the
  testbed deploy script mints at `testbed/.ssh/jaco` (else `~/.ssh/jaco`).
- Node addressing via `BENCH_PUBLIC_IPS`/`BENCH_PRIVATE_IPS` (node-1 first) or
  `RESOURCE_GROUP` (+ `az`) to resolve them.

## Run

```sh
export BENCH_PUBLIC_IPS="<n1> <n2> <n3>" BENCH_PRIVATE_IPS="<p1> <p2> <p3>"
./run.sh jaco --repeat 5       # bootstrap JACO, deploy, warm-up, 5 measured samples
./collect.sh                   # add per-node docker stats + tar the run
./scorecard.sh                 # print the comparison (rps mean ±95% CI; needs ≥2 stacks)
```

Each run does a discarded warm-up, then `--repeat N` measured k6 samples, and
records every sample plus mean/stdev/95% CI — so a single noisy run isn't
over-read (overlapping CIs ⇒ a tie). It also samples the load-generator's own CPU
(flags >80% saturation) and the idle node overhead (control-plane footprint
proxy). See [`docs/benchmarking-methodology.md`](docs/benchmarking-methodology.md).

Knobs: `BENCH_REPEAT` (1; or `--repeat N`), `BENCH_WARMUP_DURATION` (`20s`, `0`
off), `BENCH_TARGET` (default `https://jaco.sh`), `BENCH_HOST_HEADER`,
`BENCH_VUS` (20), `BENCH_DURATION` (60s), `BENCH_RW_RATIO` (5 → 20% writes),
`BENCH_READY_TIMEOUT` (420s). Keep them constant across the stacks you compare.

## Adding a stack

Each adapter in `adapters/<stack>.sh` defines four functions —
`adapter_deploy`, `adapter_collect <dir>`, `adapter_teardown`, `adapter_label`.
`run.sh` supplies the generic, identical-for-everyone phases (overhead snapshot,
warm-up, repeated k6 load, load-gen monitoring, scoring). All four adapters
(JACO, kubeadm, k3s, Swarm) are implemented — see each stack's README.
