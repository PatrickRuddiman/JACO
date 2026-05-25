# bench — the grading harness

Deploy the [shared workload](../workload) on each orchestrator, measure it the
same way every time, and produce a comparative scorecard. See
[`RUBRIC.md`](RUBRIC.md) for what's graded and how bias is controlled.

## Layout

```
bench/
├── RUBRIC.md          # dimensions, weights, fairness rules, scoring
├── run.sh             # deploy + measure + record ONE stack
├── collect.sh         # snapshot node resource stats + bundle a run for download
├── scorecard.sh       # cross-stack comparison from results/*/result.json
├── lib/common.sh      # shared helpers (node resolution, ssh, ready-poll, timing)
├── loadgen/scenario.js# the one k6 scenario every stack is measured with
├── adapters/          # per-stack deploy/collect/teardown (jaco real; others stub)
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
./run.sh jaco                  # bootstrap JACO, deploy, load-test, record
./collect.sh                   # add per-node docker stats + tar the run
./scorecard.sh                 # print the comparison (needs ≥2 stacks to rank)
```

Knobs: `BENCH_TARGET` (default `https://jaco.sh`), `BENCH_HOST_HEADER`,
`BENCH_VUS` (20), `BENCH_DURATION` (60s), `BENCH_RW_RATIO` (5 → 20% writes),
`BENCH_READY_TIMEOUT` (420s). Keep them constant across the stacks you compare.

## Adding a stack

Each adapter in `adapters/<stack>.sh` defines four functions —
`adapter_deploy`, `adapter_collect <dir>`, `adapter_teardown`, `adapter_label`.
`run.sh` supplies the generic, identical-for-everyone phases (ready-poll, k6
load, scoring). The k8s/k3s/swarm adapters are stubs today; their bootstrap and
manifests land in the follow-up pass (see each stack's README).
