# BUG 011 — publishSelf doesn't retry; follower observations never reach leader

## Symptom

After containers come up on all 3 nodes, `jaco status hello` shows
only `hello-web-0` in the Replicas table — the replicas on jaco-2 and
jaco-3 (hello-web-1 and hello-web-2) never appear in state.

Daemon logs on each follower repeat per restart:
```
publishSelf: no leader gRPC address known
```

## Severity

Medium. Containers run; operator visibility is broken. Restart policy
and rollouts depend on observed health, so this also blocks those
flows for follower-hosted replicas.

## Root cause

Two chickens, one egg:

1. `ClusterInit` FSM handler populates `Node.Address` (raft addr) but
   NOT `Node.GrpcAddress`. The leader's grpc_address is left empty.
2. `publishSelf` runs once at OpenRaft. On the leader it succeeds via
   `raft.Apply`, populating its own grpc_address. On followers it
   needs to dial `leaderGRPCAddr(...)` which looks up Node.GrpcAddress
   — empty (because step 1 didn't set it OR because the leader's
   publishSelf hasn't replicated yet). Follower's publishSelf gives up
   silently after the first attempt.

The follower's runtime SubmitFn (ReplicaObserved forwarding) hits the
same path — `leaderGRPCAddr` returns empty → forward fails → no
observation ever lands at the leader.

## Fix

(a) ClusterInit's FSM handler accepts a `SelfGrpcAddress` field and
    populates `Node.GrpcAddress` so the leader's own row is
    immediately complete after Init. The bootstrap path passes it
    in.

(b) `publishSelf` runs on a retry loop (200ms → 1s → 2s → 5s, cap
    5s) until it succeeds. Backs the Server's WaitGroup so Stop
    cancels it cleanly.

Shipping both fixes together. (b) alone would be enough for
correctness, but (a) avoids the leader-also-not-published transient
window during which follower forwards return "no leader grpc
address" errors.

## Status

**FIXING NOW.**
