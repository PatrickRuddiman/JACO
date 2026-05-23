Parent slice: [daemon](../slices/daemon.md)
Depends on: 38

# Task 41 — cross-host-tls

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Replace iter 12's plaintext TCP listener + iter 18's plaintext peer dial with TLS-with-cluster-CA. Pre-Init the daemon serves a self-signed bootstrap cert (the joiner's `Cluster.Join` dials with `InsecureSkipVerify` because the `join_token` is the trust anchor); post-Init it rebinds the same listener with the CA-signed node cert + verifies peer certs against the cluster CA. CLI gets a `--ca-cert` revival.

## Tasks
- [ ] `internal/daemon/grpc/tls.go`: new file with `bootstrapTLSConfig()` returning a `*tls.Config` whose `Certificates` is a freshly-generated self-signed ECDSA P-256 keypair valid for 24h, CN matching the daemon's hostname. Used at daemon-construction time before any cluster CA exists on disk.
- [ ] `internal/daemon/grpc/tls.go`: `clusterTLSConfig(dataDir, hostname string) (*tls.Config, error)` — loads the CA cert from `$dataDir/node/ca.crt`, the node's signed cert + key from `$dataDir/node/<hostname>.crt` and `.key`. Returns a `*tls.Config` with `ClientAuth: tls.VerifyClientCertIfGiven` (raft + Internal callers eventually pin via mTLS; v0 keeps it permissive). Errors when the files are missing — used post-Init only.
- [ ] `internal/daemon/grpc/server.go:118`: where the TCP listener opens today (plaintext), wrap with `tls.NewListener` using `bootstrapTLSConfig` so the daemon comes up TLS from byte zero.
- [ ] `internal/daemon/grpc/server.go:Server.rebindTLS`: new method called from `OpenRaft` after the node cert is on disk. Closes the existing TCP listener and re-opens it wrapped in `clusterTLSConfig`. Returns the new listener so `Serve` can swap it. The unix-socket listener is untouched (no TLS on local control).
- [ ] `internal/daemon/grpc/cluster.go:150` (the `Cluster.Join` peer dial): switch back from iter 18's `insecure.NewCredentials()` to `credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})` — pre-Init the joiner can't verify the peer's bootstrap cert, but `join_token` body auth still gates the call. Once `Cluster.NodeJoin` returns the cluster CA, subsequent ops dial with that CA pinned.
- [ ] `cmd/jaco/node.go:215` (`dialServer`): re-add CA-cert handling. When the operator passes `--ca-cert <path>`, dial with `credentials.NewTLS(&tls.Config{RootCAs: pool})`. When omitted, fall back to `InsecureSkipVerify` so the v0 muscle-memory still works but with a one-line WARN logged to stderr.
- [ ] `internal/daemon/grpc/server_test.go`: rewrite `TestServer_TCPListenerServesClusterStatus` to dial with `InsecureSkipVerify` (pre-Init still works) and add `TestServer_RebindsTLSAfterInit` that drives Init and asserts the post-Init listener presents the CA-signed cert (parse `ConnectionState().PeerCertificates[0]` and assert `Issuer.CommonName == "JACO Cluster CA"`).

## Acceptance criteria
- [ ] `go build ./...` exits 0.
- [ ] `go test ./internal/daemon/grpc/... -race -count=1` exits 0.
- [ ] `go test ./... -race -count=1` exits 0 across the whole tree.
- [ ] `git grep -nE 'tls\.NewListener' internal/daemon/grpc/` matches.
- [ ] `git grep -nE 'bootstrapTLSConfig|clusterTLSConfig' internal/daemon/grpc/tls.go` matches both.
- [ ] `git grep -nE 'InsecureSkipVerify' internal/daemon/grpc/cluster.go` matches (the documented Cluster.Join dial).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
