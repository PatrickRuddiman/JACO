# BUG 012 — Internal.Submit forwarding dials plaintext; leader listener is TLS

## Symptom

Follower observations never reach the leader. SubmitFn (after bug 011
diagnostic logging) emits:
```
submit Internal.Submit to 100.96.111.6:7000: rpc error:
  code = Unavailable desc = connection error:
  desc = "error reading server preface: connection reset by peer"
```

Same for jaco-3. The follower-to-leader gRPC handshake gets reset
because the follower opens a plaintext H2 connection but the leader's
listener is TLS-wrapped (per the task-41 cross-host TLS work).

## Severity

Medium. Containers run on every node; the operator-visible status
only ever shows leader-side observations.

## Root cause

The runtime SubmitFn in `Server.startSubsystems` was written under the
v0 plaintext model (iter 24 of task 38). When task 41 / iter 41 added
the bootstrap+cluster TLS listener, the SubmitFn never got the matching
dial-side change. Same gap exists in publishSelf forwarding and the
Deploy.Logs cross-host fanout.

## Fix

Dial Internal.Submit with TLS skip-verify (`tls.Config{
InsecureSkipVerify: true}`). The join_token-equivalent isn't required
here because Internal.Submit is in `UnauthMethods` and the body is the
raft.Apply payload — TLS-skip-verify-with-token-in-body matches the
existing Cluster.Join pattern.

Same fix lands in `publishSelf` forwarding + `streamDeploymentLogs`
peer dial.

## Status

**FIXING NOW.**
