import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { hostname } from "node:os";
import { createClient, type RedisClientType } from "redis";
import pkg from "pg";
const { Pool } = pkg;
type PgPool = InstanceType<typeof Pool>;

// ---------------------------------------------------------------------------
// Config. The app writes to REDIS_PRIMARY and reads from REDIS_REPLICA so the
// benchmark exercises primary→replica replication and the orchestrator's
// service load-balancing (REDIS_REPLICA resolves to N replica instances).
//
// Postgres is optional and used only to measure cross-node streaming
// replication: when PG_PRIMARY/PG_REPLICA are set, the api drives a heartbeat
// and publishes the primary→replica replay lag (bench_pg_replica_lag_seconds).
// ---------------------------------------------------------------------------
const REDIS_PRIMARY = process.env.REDIS_PRIMARY ?? "redis-primary:6379";
const REDIS_REPLICA = process.env.REDIS_REPLICA ?? "redis-replica:6379";
const PG_PRIMARY = process.env.PG_PRIMARY ?? "";
const PG_REPLICA = process.env.PG_REPLICA ?? "";
const PG_DB = process.env.PG_DB ?? "bench";
const PG_USER = process.env.PG_USER ?? "bench";
const PG_PASSWORD = process.env.PG_PASSWORD ?? "";
const LISTEN_PORT = Number(process.env.LISTEN_PORT ?? 8080);
const INSTANCE = process.env.HOSTNAME || hostname();

const NOTES_KEY = "notes";          // list of JSON notes, newest at head
const SEQ_KEY = "notes:seq";        // monotonic id counter
const HEARTBEAT_KEY = "bench:heartbeat"; // primary→replica lag probe
const MAX_NOTES = 1000;

interface Note {
  id: number;
  text: string;
  created_at: string;
}

// ---------------------------------------------------------------------------
// Minimal dependency-free Prometheus metrics. These are the numbers the bench
// harness scrapes ("the stack reports its own metrics"): HTTP latency,
// per-op Redis latency, and observed replication lag.
// ---------------------------------------------------------------------------
function labelKey(names: string[], labels: Record<string, string>): string {
  return names.map((n) => labels[n] ?? "").join("\x1f");
}

function escapeLabel(v: string): string {
  return v.replace(/\\/g, "\\\\").replace(/\n/g, "\\n").replace(/"/g, '\\"');
}

function fmtLabels(names: string[], labels: Record<string, string>): string {
  if (names.length === 0) return "";
  const parts = names.map((n) => `${n}="${escapeLabel(labels[n] ?? "")}"`);
  return `{${parts.join(",")}}`;
}

class Counter {
  private series = new Map<string, { labels: Record<string, string>; value: number }>();
  constructor(readonly name: string, readonly help: string, readonly labelNames: string[]) {}
  inc(labels: Record<string, string>, by = 1): void {
    const k = labelKey(this.labelNames, labels);
    const s = this.series.get(k);
    if (s) s.value += by;
    else this.series.set(k, { labels, value: by });
  }
  render(): string {
    const lines = [`# HELP ${this.name} ${this.help}`, `# TYPE ${this.name} counter`];
    for (const s of this.series.values()) {
      lines.push(`${this.name}${fmtLabels(this.labelNames, s.labels)} ${s.value}`);
    }
    return lines.join("\n");
  }
}

class Gauge {
  private series = new Map<string, { labels: Record<string, string>; value: number }>();
  constructor(readonly name: string, readonly help: string, readonly labelNames: string[]) {}
  set(labels: Record<string, string>, value: number): void {
    this.series.set(labelKey(this.labelNames, labels), { labels, value });
  }
  render(): string {
    const lines = [`# HELP ${this.name} ${this.help}`, `# TYPE ${this.name} gauge`];
    for (const s of this.series.values()) {
      lines.push(`${this.name}${fmtLabels(this.labelNames, s.labels)} ${s.value}`);
    }
    return lines.join("\n");
  }
}

class Histogram {
  private series = new Map<
    string,
    { labels: Record<string, string>; counts: number[]; sum: number; count: number }
  >();
  constructor(
    readonly name: string,
    readonly help: string,
    readonly buckets: number[],
    readonly labelNames: string[],
  ) {}
  observe(labels: Record<string, string>, v: number): void {
    const k = labelKey(this.labelNames, labels);
    let s = this.series.get(k);
    if (!s) {
      s = { labels, counts: new Array(this.buckets.length).fill(0), sum: 0, count: 0 };
      this.series.set(k, s);
    }
    s.count += 1;
    s.sum += v;
    // Cumulative ("le") buckets: every bound >= v gets the observation.
    for (let i = 0; i < this.buckets.length; i++) {
      if (v <= this.buckets[i]) s.counts[i] += 1;
    }
  }
  render(): string {
    const lines = [`# HELP ${this.name} ${this.help}`, `# TYPE ${this.name} histogram`];
    for (const s of this.series.values()) {
      for (let i = 0; i < this.buckets.length; i++) {
        const l = { ...s.labels, le: String(this.buckets[i]) };
        lines.push(`${this.name}_bucket${fmtLabels([...this.labelNames, "le"], l)} ${s.counts[i]}`);
      }
      const inf = { ...s.labels, le: "+Inf" };
      lines.push(`${this.name}_bucket${fmtLabels([...this.labelNames, "le"], inf)} ${s.count}`);
      lines.push(`${this.name}_sum${fmtLabels(this.labelNames, s.labels)} ${s.sum}`);
      lines.push(`${this.name}_count${fmtLabels(this.labelNames, s.labels)} ${s.count}`);
    }
    return lines.join("\n");
  }
}

const HTTP_BUCKETS = [0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10];
const REDIS_BUCKETS = [0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1];

const httpRequests = new Counter("bench_http_requests_total", "HTTP requests handled.", ["method", "route", "code"]);
const httpDuration = new Histogram("bench_http_request_duration_seconds", "HTTP request latency.", HTTP_BUCKETS, ["method", "route"]);
const redisDuration = new Histogram("bench_redis_op_duration_seconds", "Redis op latency.", REDIS_BUCKETS, ["op", "role"]);
const redisErrors = new Counter("bench_redis_errors_total", "Redis ops that errored.", ["op", "role"]);
const replicaLag = new Gauge("bench_replica_lag_seconds", "Observed Redis primary→replica lag from the heartbeat probe.", []);
const buildInfo = new Gauge("bench_build_info", "Static info; value is always 1.", ["instance"]);
const redisUp = new Gauge("bench_redis_up", "1 if the role's last op succeeded, else 0.", ["role"]);
const pgReplicaLag = new Gauge("bench_pg_replica_lag_seconds", "Postgres primary→replica streaming replay lag (cross-node), from pg_stat_replication.", []);
const pgUp = new Gauge("bench_pg_up", "1 if the Postgres role's last query succeeded, else 0.", ["role"]);

buildInfo.set({ instance: INSTANCE }, 1);

function renderMetrics(): string {
  return (
    [
      httpRequests,
      httpDuration,
      redisDuration,
      redisErrors,
      replicaLag,
      redisUp,
      pgReplicaLag,
      pgUp,
      buildInfo,
    ]
      .map((m) => m.render())
      .join("\n\n") + "\n"
  );
}

// Time a redis op, recording latency + up/down for the given role.
async function timedRedis<T>(op: string, role: "primary" | "replica", fn: () => Promise<T>): Promise<T> {
  const start = process.hrtime.bigint();
  try {
    const out = await fn();
    redisUp.set({ role }, 1);
    return out;
  } catch (err) {
    redisErrors.inc({ op, role });
    redisUp.set({ role }, 0);
    throw err;
  } finally {
    const sec = Number(process.hrtime.bigint() - start) / 1e9;
    redisDuration.observe({ op, role }, sec);
  }
}

// ---------------------------------------------------------------------------
// Redis clients.
// ---------------------------------------------------------------------------
function newClient(addr: string): RedisClientType {
  const client: RedisClientType = createClient({
    url: `redis://${addr}`,
    socket: {
      reconnectStrategy: (retries) => Math.min(retries * 100, 2000),
      connectTimeout: 5000,
    },
  });
  client.on("error", (err) => console.error(`redis ${addr} error: ${(err as Error).message}`));
  return client;
}

const primary = newClient(REDIS_PRIMARY);
const replica = newClient(REDIS_REPLICA);

async function connectWithRetry(client: RedisClientType, label: string): Promise<void> {
  for (let i = 0; i < 60; i++) {
    try {
      await client.connect();
      console.log(`connected to ${label}`);
      return;
    } catch (err) {
      console.log(`${label} not ready (${(err as Error).message}); retrying in 1s`);
      await new Promise((r) => setTimeout(r, 1000));
    }
  }
  throw new Error(`timed out connecting to ${label}`);
}

// ---------------------------------------------------------------------------
// HTTP helpers.
// ---------------------------------------------------------------------------
function sendJson(res: ServerResponse, status: number, body: unknown): void {
  res.statusCode = status;
  res.setHeader("Content-Type", "application/json");
  res.setHeader("Cache-Control", "no-store");
  res.end(JSON.stringify(body));
}

function sendText(res: ServerResponse, status: number, body: string): void {
  res.statusCode = status;
  res.setHeader("Content-Type", "text/plain; charset=utf-8");
  res.end(body);
}

async function readJson(req: IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = [];
  let total = 0;
  for await (const chunk of req) {
    const buf = chunk as Buffer;
    total += buf.length;
    if (total > 64 * 1024) throw new Error("payload too large");
    chunks.push(buf);
  }
  const raw = Buffer.concat(chunks).toString("utf8");
  return raw.length === 0 ? {} : JSON.parse(raw);
}

async function listNotes(res: ServerResponse): Promise<void> {
  const raw = await timedRedis("lrange", "replica", () => replica.lRange(NOTES_KEY, 0, 99));
  const notes: Note[] = [];
  for (const s of raw) {
    try {
      notes.push(JSON.parse(s) as Note);
    } catch {
      /* skip malformed */
    }
  }
  sendJson(res, 200, notes);
}

async function createNote(req: IncomingMessage, res: ServerResponse): Promise<void> {
  let body: unknown;
  try {
    body = await readJson(req);
  } catch {
    sendText(res, 400, "invalid json");
    return;
  }
  const text =
    typeof body === "object" && body !== null && "text" in body
      ? (body as { text: unknown }).text
      : undefined;
  if (typeof text !== "string" || text.length === 0) {
    sendText(res, 400, "text is required");
    return;
  }
  const id = await timedRedis("incr", "primary", () => primary.incr(SEQ_KEY));
  const note: Note = { id, text, created_at: new Date().toISOString() };
  await timedRedis("lpush", "primary", async () => {
    await primary.lPush(NOTES_KEY, JSON.stringify(note));
    await primary.lTrim(NOTES_KEY, 0, MAX_NOTES - 1);
  });
  sendJson(res, 201, note);
}

// Route label is the matched template (not the raw path) so metric cardinality
// stays bounded.
function routeLabel(method: string, path: string): string {
  if (path === "/notes") return "/notes";
  if (path === "/healthz") return "/healthz";
  if (path === "/metrics") return "/metrics";
  return "other";
}

async function dispatch(req: IncomingMessage, res: ServerResponse, path: string): Promise<void> {
  if (path === "/healthz") {
    try {
      await primary.ping();
      sendText(res, 200, "ok");
    } catch {
      sendText(res, 503, "redis down");
    }
    return;
  }
  if (path === "/metrics") {
    res.statusCode = 200;
    res.setHeader("Content-Type", "text/plain; version=0.0.4; charset=utf-8");
    res.end(renderMetrics());
    return;
  }
  if (path === "/notes") {
    if (req.method === "GET") return listNotes(res);
    if (req.method === "POST") return createNote(req, res);
    sendText(res, 405, "method not allowed");
    return;
  }
  sendText(res, 404, "not found");
}

async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const url = new URL(req.url ?? "/", "http://localhost");
  const path = url.pathname;
  const method = req.method ?? "GET";
  const route = routeLabel(method, path);
  const start = process.hrtime.bigint();
  res.on("finish", () => {
    const sec = Number(process.hrtime.bigint() - start) / 1e9;
    httpDuration.observe({ method, route }, sec);
    httpRequests.inc({ method, route, code: String(res.statusCode) });
  });
  await dispatch(req, res, path);
}

// Heartbeat loop: stamp the primary, read it from a replica, expose the delta
// as observed replication lag. Skips /metrics-scrape coupling so the gauge is
// always fresh regardless of request traffic.
function startHeartbeat(): NodeJS.Timeout {
  return setInterval(() => {
    void (async () => {
      try {
        const now = Date.now();
        await primary.set(HEARTBEAT_KEY, String(now));
        const seen = await replica.get(HEARTBEAT_KEY);
        if (seen !== null) {
          const lag = (Date.now() - Number(seen)) / 1000;
          replicaLag.set({}, lag >= 0 ? lag : 0);
        }
      } catch {
        /* transient; error counters already track op failures */
      }
    })();
  }, 1000);
}

// --- Postgres streaming-replication probe (optional) -----------------------
function newPgPool(addr: string): PgPool {
  const [host, portStr] = addr.split(":");
  return new Pool({
    host,
    port: Number(portStr || "5432"),
    database: PG_DB,
    user: PG_USER,
    password: PG_PASSWORD || undefined,
    max: 4,
    connectionTimeoutMillis: 5000,
  });
}

async function pgConnectWithRetry(pool: PgPool, label: string): Promise<void> {
  for (let i = 0; i < 120; i++) {
    try {
      await pool.query("SELECT 1");
      console.log(`connected to ${label}`);
      return;
    } catch (err) {
      console.log(`${label} not ready (${(err as Error).message}); retrying in 1s`);
      await new Promise((r) => setTimeout(r, 1000));
    }
  }
  throw new Error(`timed out connecting to ${label}`);
}

async function pgEnsureSchema(pgPrimary: PgPool): Promise<void> {
  await pgPrimary.query(
    "CREATE TABLE IF NOT EXISTS bench_heartbeat (id int PRIMARY KEY, ts timestamptz NOT NULL DEFAULT clock_timestamp())",
  );
  await pgPrimary.query(
    "INSERT INTO bench_heartbeat (id, ts) VALUES (1, clock_timestamp()) ON CONFLICT (id) DO NOTHING",
  );
}

// Write a timestamp on the primary every second (generating WAL), then read the
// primary→replica replay lag straight from pg_stat_replication — Postgres' own
// measurement, so no cross-node clock skew enters the number. Primary and
// replica are pinned to different nodes, so this is mesh replication time.
function startPgHeartbeat(pgPrimary: PgPool, pgReplica: PgPool): NodeJS.Timeout {
  return setInterval(() => {
    void (async () => {
      try {
        await pgPrimary.query("UPDATE bench_heartbeat SET ts = clock_timestamp() WHERE id = 1");
        const r = await pgPrimary.query(
          "SELECT EXTRACT(EPOCH FROM replay_lag)::float8 AS lag FROM pg_stat_replication ORDER BY replay_lag DESC NULLS LAST LIMIT 1",
        );
        pgUp.set({ role: "primary" }, 1);
        const lag = r.rows[0]?.lag;
        if (typeof lag === "number" && Number.isFinite(lag)) {
          pgReplicaLag.set({}, lag >= 0 ? lag : 0);
        }
      } catch {
        pgUp.set({ role: "primary" }, 0);
      }
      try {
        await pgReplica.query("SELECT 1");
        pgUp.set({ role: "replica" }, 1);
      } catch {
        pgUp.set({ role: "replica" }, 0);
      }
    })();
  }, 1000);
}

async function main(): Promise<void> {
  await connectWithRetry(primary, `primary ${REDIS_PRIMARY}`);
  await connectWithRetry(replica, `replica ${REDIS_REPLICA}`);

  const heartbeat = startHeartbeat();

  const server = createServer((req, res) => {
    handle(req, res).catch((err: unknown) => {
      const msg = err instanceof Error ? err.message : String(err);
      console.error(`request failed: ${msg}`);
      if (!res.headersSent) sendText(res, 500, "internal error");
      else res.end();
    });
  });

  server.listen(LISTEN_PORT, () => console.log(`api listening on :${LISTEN_PORT} (instance ${INSTANCE})`));

  // Postgres is a measurement-only side tier — set it up in the BACKGROUND so a
  // slow/absent replica never blocks the HTTP server (and thus the healthcheck).
  let pgHeartbeat: NodeJS.Timeout | undefined;
  let pgPrimaryPool: PgPool | undefined;
  let pgReplicaPool: PgPool | undefined;
  if (PG_PRIMARY && PG_REPLICA) {
    void (async () => {
      try {
        pgPrimaryPool = newPgPool(PG_PRIMARY);
        pgReplicaPool = newPgPool(PG_REPLICA);
        await pgConnectWithRetry(pgPrimaryPool, `pg primary ${PG_PRIMARY}`);
        await pgConnectWithRetry(pgReplicaPool, `pg replica ${PG_REPLICA}`);
        await pgEnsureSchema(pgPrimaryPool);
        pgHeartbeat = startPgHeartbeat(pgPrimaryPool, pgReplicaPool);
        console.log(`postgres replication probe enabled (primary ${PG_PRIMARY}, replica ${PG_REPLICA})`);
      } catch (err) {
        console.error(`postgres probe disabled: ${(err as Error).message}`);
      }
    })();
  }

  for (const signal of ["SIGINT", "SIGTERM"] as const) {
    process.on(signal, () => {
      console.log(`${signal} received, shutting down`);
      clearInterval(heartbeat);
      if (pgHeartbeat) clearInterval(pgHeartbeat);
      server.close(() => {
        Promise.allSettled([
          primary.quit(),
          replica.quit(),
          pgPrimaryPool?.end() ?? Promise.resolve(),
          pgReplicaPool?.end() ?? Promise.resolve(),
        ]).finally(() => process.exit(0));
      });
    });
  }
}

main().catch((err: unknown) => {
  console.error("fatal:", err);
  process.exit(1);
});
