# BUG 013 — streamLocalLogs reads test-override hostname; empty in production

## Symptom

`jaco logs hello/web` against the live cluster returns:
```
rpc error: code = Internal desc = logs_stream:
  rpc error: code = Internal desc = hostname_not_resolved
```

## Severity

Blocking for the operator-facing logs path on a real production
daemon. The DEAMON's local in-process tests pass because they set
the test override.

## Root cause

`internal/daemon/grpc/deploy_logs.go::streamLocalLogs` reads
`hostname := s.cluster.hostname`. That field is the test-only override
on `clusterServer`; production leaves it empty and resolves through
`effectiveHostname()` (which calls `os.Hostname()` when the override
is unset).

Same gap in `streamDeploymentLogs`.

## Fix

Replace the bare `s.cluster.hostname` reads with
`s.cluster.effectiveHostname()` calls in both functions. Errors there
should surface as `Internal` codes rather than the current
hostname_not_resolved branch.

## Status

**FIXING NOW.**
