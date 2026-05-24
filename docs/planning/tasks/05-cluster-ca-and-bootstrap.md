Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 04

# Task 05 — cluster-ca-and-bootstrap

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
First-node bootstrap: generate the cluster id, cluster CA, this node's signed server cert, and the initial operator token; raft-bootstrap as a single voter; print the operator token once.

## Tasks
- [x] Create `internal/controlplane/ca/ca.go` exposing `GenerateClusterCA() (caCertPEM, caKeyPEM []byte, err error)` using Ed25519, valid ~10 years; plus `ParseCA(...)` for downstream consumers.
- [x] Create `internal/controlplane/ca/node_cert.go` exposing `GenerateNodeKeypair(hostname string) (keyPEM, csrPEM []byte, err error)` and `SignNodeCSR(csrPEM, caCertPEM, caKeyPEM []byte) (nodeCertPEM []byte, err error)`. Subject CN + DNS SAN (or IP SAN if hostname parses as an IP) carry the hostname; KeyUsage covers TLS server + client auth.
- [x] Create `internal/controlplane/bootstrap/bootstrap.go` (the orchestration; isolated from cobra so it's unit-testable). `Run(Options{DataDir, Name, BindAddr, LogOut})` performs the full bootstrap and returns `{ClusterID, OperatorToken}`. Refuses to proceed if `${DataDir}/raft/log.db` already exists. Writes `${DataDir}/node/<name>.{key,crt}` (0600/0644) plus `${DataDir}/node/ca.crt`. Boots raft via `raftnode.New(Bootstrap:true)`, waits for leadership, and raft-Applies `Command{ClusterInit}` carrying the cluster_id (UUIDv4), CA cert + key, and the SHA-256-hashed operator token.
- [x] Operator token: 32 bytes of `crypto/rand`, hex-encoded; SHA-256 hash stored under `Token{identity:"bootstrap", hashed_secret}` via the ClusterInit Command. The cleartext token is returned exactly once and is never persisted to disk or logged.
- [x] Create `cmd/jaco/bootstrap.go` cobra wrapper for `jaco bootstrap --name <hostname>`. Reads `JACO_DATA_DIR` (default `/var/lib/jaco`); calls `bootstrap.Run`; prints exactly one line: `Operator token (save this; not recoverable): <hex>`.
- [x] Create `internal/controlplane/ca/ca_test.go` and `node_cert_test.go`: CA self-signed-validity + 10-year duration + Ed25519 type assertion; node cert chains to CA via `x509.Verify` (including DNS-SAN match); IP-hostname path produces IPAddresses; tampered CSR rejected. Plus `bootstrap_test.go`: artifact existence + file mode 0600 + 64-char hex token + UUID-v4 shape + refuses to overwrite existing state + required-fields validation + token hash round-trip.

## Acceptance criteria
- [x] `go test ./internal/controlplane/ca/... -race -count=1` exits 0.
- [x] `go build -o jaco ./cmd/jaco && JACO_DATA_DIR=$(mktemp -d) ./jaco bootstrap --name testhost` exits 0 and prints exactly one line starting with `Operator token`.
- [x] After bootstrap, `test -f $JACO_DATA_DIR/raft/log.db && test -f $JACO_DATA_DIR/node/testhost.crt && test -f $JACO_DATA_DIR/node/testhost.key`.
- [x] `stat -c '%a' $JACO_DATA_DIR/node/testhost.key` prints `600`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
