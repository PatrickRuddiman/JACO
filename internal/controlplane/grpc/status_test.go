package grpcsrv_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// applyDeployment is a convenience that drives Deploy.Apply through the
// existing cluster's gRPC client.
func applyDeployment(t *testing.T, c *twoNodeCluster, jacoYAML, composeYAML string) {
	t.Helper()
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"authorization", "Bearer "+c.OperatorToken)
	conn := dialConn(t, c.A.Server.Addr().String(), c.A.CACert, "node-a")
	t.Cleanup(func() { _ = conn.Close() })
	deploy := pb.NewDeployClient(conn)
	if _, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(jacoYAML),
		ComposeYaml: []byte(composeYAML),
	}); err != nil {
		t.Fatalf("Deploy.Apply: %v", err)
	}
}

func newStatusClient(t *testing.T, c *twoNodeCluster) pb.DeployClient {
	t.Helper()
	conn := dialConn(t, c.A.Server.Addr().String(), c.A.CACert, "node-a")
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewDeployClient(conn)
}

func newWatchClient(t *testing.T, c *twoNodeCluster) pb.WatchClient {
	t.Helper()
	conn := dialConn(t, c.A.Server.Addr().String(), c.A.CACert, "node-a")
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewWatchClient(conn)
}

const statusJacoYAML = `deployment: sample
services:
  - name: web
    replicas: 2
routes:
  - domain: web.example.com
    service: web
    port: 80
    tls: auto
`

const statusComposeYAML = `services:
  web:
    image: nginx:1.27
networks:
  default: {}
`

func TestStatus_ReturnsDeploymentAndRoutes(t *testing.T) {
	c := setupTwoNodeCluster(t)
	applyDeployment(t, c, statusJacoYAML, statusComposeYAML)

	client := newStatusClient(t, c)
	resp, err := client.Status(authContext(c.OperatorToken), &pb.DeployStatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(resp.GetDeployments()) != 1 {
		t.Errorf("deployments = %d, want 1", len(resp.GetDeployments()))
	}
	if got := resp.GetDeployments()[0].GetName(); got != "sample" {
		t.Errorf("deployment name = %q", got)
	}
	if len(resp.GetRoutes()) != 1 {
		t.Errorf("routes = %d, want 1", len(resp.GetRoutes()))
	}
}

func TestStatus_FiltersByDeployment(t *testing.T) {
	c := setupTwoNodeCluster(t)
	applyDeployment(t, c, statusJacoYAML, statusComposeYAML)

	otherYAML := `deployment: other
services:
  - name: web
    replicas: 1
`
	applyDeployment(t, c, otherYAML, statusComposeYAML)

	client := newStatusClient(t, c)
	resp, err := client.Status(authContext(c.OperatorToken), &pb.DeployStatusRequest{DeploymentFilter: "sample"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(resp.GetDeployments()) != 1 || resp.GetDeployments()[0].GetName() != "sample" {
		t.Errorf("filtered deployments = %+v, want [sample]", resp.GetDeployments())
	}
}

func TestStatusWatch_StreamsUpdatesAcrossSnapshots(t *testing.T) {
	// The AC: when a deployment changes, the watcher receives at least one
	// event per change. Each Apply produces a Deployment{Updated} (or
	// {Added}) event on the watch stream — so after two Apply calls we
	// should see at least 2 events.
	c := setupTwoNodeCluster(t)
	wc := newWatchClient(t, c)

	streamCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := wc.Subscribe(authContextStream(c.OperatorToken, streamCtx), &pb.SubscribeRequest{
		EntityTypes:      []string{"deployments", "replicas_observed", "routes"},
		DeploymentFilter: "sample",
	})
	if err != nil {
		t.Fatalf("Watch.Subscribe: %v", err)
	}

	got := make(chan *pb.SubscribeEvent, 16)
	errs := make(chan error, 1)
	go func() {
		for {
			ev, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				errs <- err
				return
			}
			got <- ev
		}
	}()

	// Trigger a few changes — two deployment applies will emit events.
	applyDeployment(t, c, statusJacoYAML, statusComposeYAML)
	applyDeployment(t, c, statusJacoYAML, statusComposeYAML)

	// Wait for at least 2 events.
	deadline := time.Now().Add(3 * time.Second)
	count := 0
	for count < 2 && time.Now().Before(deadline) {
		select {
		case <-got:
			count++
		case err := <-errs:
			t.Fatalf("stream Recv: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
	}
	if count < 2 {
		t.Fatalf("watch events received = %d, want >= 2", count)
	}
}

func authContextStream(token string, base context.Context) context.Context {
	return metadata.AppendToOutgoingContext(base, "authorization", "Bearer "+token)
}
