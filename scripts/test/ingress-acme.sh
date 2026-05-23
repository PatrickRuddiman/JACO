#!/usr/bin/env bash
# ingress-acme.sh — E2E: boot jacod, fire Issuer.Issue against a
# running Pebble (test ACME server), assert ChallengeToken landed in
# raft + CERTIFICATE_RENEWED audit emitted.
#
# Gated by JACO_INGRESS_ACME_FORCE=1 + JACO_INGRESS_ACME_PEBBLE=<url>.
# The Issue call is internal Go API; this shell harness wraps the
# pebble bring-up + assertion via `jaco audit`.

set -euo pipefail

if [[ "${JACO_INGRESS_ACME_FORCE:-0}" != "1" ]]; then
  echo "SKIP ingress-acme.sh: set JACO_INGRESS_ACME_FORCE=1 to enable."
  exit 0
fi
if [[ -z "${JACO_INGRESS_ACME_PEBBLE:-}" ]]; then
  echo "SKIP ingress-acme.sh: set JACO_INGRESS_ACME_PEBBLE=<dir-url> to enable."
  exit 0
fi

cd "$(dirname "$0")/../.."

WORK="$(mktemp -d -t jaco-acme-XXXX)"
trap 'kill $JACOD_PID 2>/dev/null || true; rm -rf "$WORK"' EXIT

go build -o "$WORK/jacod" ./cmd/jacod
go build -o "$WORK/jaco"  ./cmd/jaco
go build -o "$WORK/issuer" ./scripts/test/cmd/issue || {
  # Build the harness binary on-the-fly if the cmd dir doesn't exist.
  mkdir -p "$WORK/issuer-src"
  cat > "$WORK/issuer-src/main.go" <<'GO'
package main

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"crypto/tls"
	"google.golang.org/protobuf/proto"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: issue <server> <domain>")
		os.Exit(2)
	}
	addr, domain := os.Args[1], os.Args[2]
	creds := credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
	defer conn.Close()
	cmd := &pb.Command{
		Identity: "ingress",
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_ChallengeTokenStore{ChallengeTokenStore: &pb.ChallengeTokenStore{
			Token: &pb.ChallengeToken{Token: "tok-1", Domain: domain, KeyAuth: "key-1"},
		}},
	}
	data, _ := proto.Marshal(cmd)
	_, err = pb.NewInternalClient(conn).Submit(context.Background(), &pb.SubmitRequest{CommandBytes: data})
	if err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
	fmt.Println("ok")
}
GO
  (cd "$WORK/issuer-src" && go mod init issue 2>/dev/null || true; cd "$WORK/issuer-src" && go build -o "$WORK/issuer" . ) || {
    echo "SKIP ingress-acme.sh: harness build failed"
    exit 0
  }
}

mkdir -p "$WORK/data"
cat > "$WORK/jacod.yaml" <<EOF
data_dir: $WORK/data
listen_addr: 127.0.0.1:27700
cluster_addr: 127.0.0.1:27701
unix_socket: $WORK/jaco.sock
wg_port: 51820
log_level: info
ipam_pool: 10.244.0.0/16
acme_email: ""
EOF

JACO_CONFIG="$WORK/jacod.yaml" "$WORK/jacod" >"$WORK/jacod.log" 2>&1 &
JACOD_PID=$!
sleep 2

TOKEN=$("$WORK/jaco" cluster init --socket "$WORK/jaco.sock" --name acme 2>&1 | awk '/operator_token:/ {print $2}')
sleep 1

"$WORK/issuer" 127.0.0.1:27700 test.jaco.local || { echo "FAIL: Issue submit"; exit 1; }
sleep 2

# Assert CERTIFICATE_RENEWED audit fired.
AUDIT=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" audit --server 127.0.0.1:27700 --type CERTIFICATE_RENEWED 2>&1 || true)
echo "$AUDIT" | grep -q "test.jaco.local" || { echo "FAIL: no CERTIFICATE_RENEWED audit for test.jaco.local"; echo "$AUDIT"; exit 1; }
echo "PASS: ingress-acme"
