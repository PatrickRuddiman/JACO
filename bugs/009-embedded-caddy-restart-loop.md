# BUG 009 — Embedded caddy admin endpoint thrashes once per second

## Symptom

`journalctl -u jaco.service` shows a tight pair every second:
```
admin endpoint started
stopped previous server
```

Burns CPU, floods logs, and drowns out useful messages from the
runtime / scheduler / discovery subsystems.

## Severity

Medium. Not fatal but masks other bugs and wastes resources. Caused
the operator (me) to miss the actual reconciler errors during the
bug006/007/008 cascade.

## Root cause

The ingress `Reloader.Run` loop subscribes to ReplicasObserved.
health.Watcher publishes a ReplicaObserved event every poll
(FastPollInterval ~ 1s when transitioning, SlowPollInterval ~ longer
once RUNNING). The Reloader debounces 200ms and then calls Rebuild
→ BuildCaddyConfig.

The expectation is that `bytes.Equal(cfg, lastCfg)` skips
`caddy.Load` when the rendered config hasn't changed. But the config
DOES change every tick — `BuildCaddyConfig` filters replicas by
`now - last_health_at < HealthFreshness` and that filter result
changes as observations age. The output bytes drift even when the
operator-visible state is "nothing happening".

Each `caddy.Load(cfg, false)` provisions a new admin endpoint and
stops the previous one — even with semantically identical configs.
That's the source of the 1Hz log spam.

## Fix

In `ingressLoaderEmbedded`, defer the first `caddy.Load` until the
rendered config carries at least one user-declared route. With zero
routes, the fallback 404 + ACME config is no-op-equivalent to "caddy
not running" so there's no reason to start the admin endpoint.

Detect "no routes" by checking whether the JSON contains
`"apps":{"http":{"servers":{"jaco":{"routes":[<more than just the
fallback static route>]}}}}`. Concretely: count occurrences of
`"reverse_proxy"` — fallback config has zero, any route config has
≥1.

When zero routes, the Loader is a no-op + returns nil so the
Reloader's `lastCfg` tracking still works.

## Status

**FIXING NOW.**
