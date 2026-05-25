// Deterministic load scenario for the orchestrator bench. The SAME script runs
// against every stack at the same ingress, so throughput/latency differences
// come from the orchestrator + network, not from the test. Tunable only via
// env so a run is fully described by its environment:
//
//   BENCH_TARGET       base URL of the ingress      (default https://jaco.sh)
//   BENCH_HOST_HEADER  Host header override          (default: none)
//   BENCH_VUS          concurrent virtual users      (default 20)
//   BENCH_DURATION     steady-state duration         (default 60s)
//   BENCH_RW_RATIO     1-in-N iterations is a write  (default 5 → 20% writes)
//
// Run via:  docker run --rm -v "$PWD":/work -e BENCH_TARGET=... \
//             grafana/k6 run /work/scenario.js --summary-export /work/summary.json
import http from "k6/http";
import { check } from "k6";
import { Trend, Counter } from "k6/metrics";

const BASE = __ENV.BENCH_TARGET || "https://jaco.sh";
const HOST = __ENV.BENCH_HOST_HEADER || "";
const VUS = Number(__ENV.BENCH_VUS || "20");
const DURATION = __ENV.BENCH_DURATION || "60s";
const RW = Number(__ENV.BENCH_RW_RATIO || "5");

const baseHeaders = HOST ? { Host: HOST } : {};

export const options = {
  // constant-vus keeps offered load identical across stacks (no auto-scaling
  // of the generator that could mask a slow stack).
  scenarios: {
    steady: { executor: "constant-vus", vus: VUS, duration: DURATION },
  },
  thresholds: {
    http_req_failed: ["rate<0.05"],
    http_req_duration: ["p(95)<2000"],
  },
  // Make p99 available in --summary-export (default stats omit it).
  summaryTrendStats: ["avg", "min", "med", "p(90)", "p(95)", "p(99)", "max"],
  // insecureSkipTLSVerify so a staging/Caddy cert never fails the run; the
  // bench measures performance, not certificate validity.
  insecureSkipTLSVerify: true,
  noConnectionReuse: false,
};

const readLatency = new Trend("bench_read_ms", true);
const writeLatency = new Trend("bench_write_ms", true);
const writes = new Counter("bench_writes");
const reads = new Counter("bench_reads");

export default function () {
  if (__ITER % RW === 0) {
    const res = http.post(
      `${BASE}/api/notes`,
      JSON.stringify({ text: `note-${__VU}-${__ITER}` }),
      { headers: { ...baseHeaders, "Content-Type": "application/json" } },
    );
    writeLatency.add(res.timings.duration);
    writes.add(1);
    check(res, { "write 2xx": (r) => r.status >= 200 && r.status < 300 });
  } else {
    const res = http.get(`${BASE}/api/notes`, { headers: baseHeaders });
    readLatency.add(res.timings.duration);
    reads.add(1);
    check(res, { "read 200": (r) => r.status === 200 });
  }
}
