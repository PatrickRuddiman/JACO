package grpc_test

import (
	"context"
	"testing"
	"time"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

const proxyTestCompose = `services:
  web:
    image: nginx:1.27
`

// TestProxies_DeployApplyReachesHandler proves the daemon registers Deploy
// and forwards through the proxy after Init populates state + raft.
func TestProxies_DeployApplyReachesHandler(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	resp, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	waitForOperatorToken(t, s)

	authCtx := withOperatorAuth(context.Background(), resp.GetOperatorToken())
	deploy := pb.NewDeployClient(conn)
	_, err = deploy.Apply(authCtx, &pb.ApplyRequest{
		ComposeYaml: []byte(proxyTestCompose),
		JacoYaml:    []byte("deployment: smoke\nservices:\n  - name: web\n    compose_service: web\n    replicas: 1\n"),
	})
	// Apply may surface a validation error (depending on what compose
	// expects), but the call must reach the controlplane handler — not
	// the proxy's "state_unavailable" fallback. We assert the err message
	// doesn't mention state_unavailable.
	if err != nil && contains(err.Error(), "state_unavailable") {
		t.Errorf("Apply hit proxy fallback: %v", err)
	}
}

// TestProxies_TokensListReachesHandler — same check for Tokens.List.
func TestProxies_TokensListReachesHandler(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	resp, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	waitForOperatorToken(t, s)

	authCtx := withOperatorAuth(context.Background(), resp.GetOperatorToken())
	tokens := pb.NewTokensClient(conn)
	_, err = tokens.List(authCtx, &pb.TokenListRequest{})
	if err != nil && contains(err.Error(), "state_unavailable") {
		t.Errorf("Tokens.List hit proxy fallback: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// silence unused
var _ = time.Now
