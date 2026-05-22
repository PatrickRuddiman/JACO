Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 04

# Task 05 — cluster-ca-and-bootstrap

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
First-node bootstrap: generate the cluster id, cluster CA, this node's signed server cert, and the initial operator token; raft-bootstrap as a single voter; print the operator token once.

## Tasks
- [ ] Create `internal/controlplane/ca/ca.go` exposing `GenerateClusterCA() (caCertPEM []byte, caKeyPEM []byte, err error)` using Ed25519, valid 10 years.
- [ ] Create `internal/controlplane/ca/node_cert.go` exposing `SignNodeCSR(csrPEM, caCertPEM, caKeyPEM []byte) (nodeCertPEM []byte, err error)` and `GenerateNodeKeypair() (keyPEM, csrPEM []byte, err error)`.
- [ ] Create `cmd/jaco/bootstrap.go` registering `jaco bootstrap --name <hostname>` cobra subcommand on `rootCmd`. Implementation: read `JACO_DATA_DIR` (default `/var/lib/jaco`); ensure it's writable; generate cluster_id (UUIDv4); generate cluster CA; generate node keypair and self-sign via the CA; write `${DATA}/node/<name>.{key,crt}` mode 0600/0644; raft-bootstrap with `Bootstrap: true`; raft-Apply `Command{ClusterInit}{cluster_id, ca_cert, ca_key, operator_token_hash}`; print exactly one line to stdout: `Operator token (save this; not recoverable): <hex>`.
- [ ] Operator token: 32 bytes of `crypto/rand`, hex-encoded; SHA-256 hash stored under `Token{identity:"bootstrap", hashed_secret:<hex>}` via the Command.
- [ ] Create `internal/controlplane/ca/ca_test.go` and `node_cert_test.go` verifying the CA→node cert chain validates via `x509.Verify`.

## Acceptance criteria
- [ ] `go test ./internal/controlplane/ca/... -race -count=1` exits 0.
- [ ] `go build -o jaco ./cmd/jaco && JACO_DATA_DIR=$(mktemp -d) ./jaco bootstrap --name testhost` exits 0 and prints exactly one line starting with `Operator token`.
- [ ] After bootstrap, `test -f $JACO_DATA_DIR/raft/log.db && test -f $JACO_DATA_DIR/node/testhost.crt && test -f $JACO_DATA_DIR/node/testhost.key`.
- [ ] `stat -c '%a' $JACO_DATA_DIR/node/testhost.key` prints `600`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
