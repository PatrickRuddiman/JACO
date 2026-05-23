package grpcsrv_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

const sampleJacoYAML = `deployment: sample
services:
  - name: web
    replicas: 3
    networks: [frontend]
  - name: api
    replicas: 2
    placement: pack
routes:
  - domain: web.example.com
    service: web
    port: 80
    tls: auto
  - domain: api.example.com
    service: api
    port: 8080
    tls: off
`

const sampleComposeYAML = `services:
  web:
    image: nginx:1.27
    networks: [frontend]
  api:
    image: api:1.0
    networks: [frontend, backend]
networks:
  frontend: {}
  backend: {}
`

func TestDeploy_ApplyDryRunLeavesStateUntouched(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	if _, ok := c.A.State.Deployments.Get("sample"); ok {
		t.Fatalf("sample exists before apply")
	}

	resp, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(sampleJacoYAML),
		ComposeYaml: []byte(sampleComposeYAML),
		DryRun:      true,
	})
	if err != nil {
		t.Fatalf("Apply(dry_run): %v", err)
	}
	if resp.GetAppliedRevision() != 0 {
		t.Errorf("dry-run applied_revision = %d, want 0 (unchanged)", resp.GetAppliedRevision())
	}
	if _, ok := c.A.State.Deployments.Get("sample"); ok {
		t.Errorf("dry-run leaked a Deployment into state")
	}
	if c.A.State.Routes.Len() != 0 {
		t.Errorf("dry-run leaked Routes; got %d", c.A.State.Routes.Len())
	}
}

func TestDeploy_ApplyWritesDeploymentAndRoutes(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	resp, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(sampleJacoYAML),
		ComposeYaml: []byte(sampleComposeYAML),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if resp.GetAppliedRevision() != 1 {
		t.Errorf("applied_revision = %d, want 1", resp.GetAppliedRevision())
	}

	dep, ok := c.A.State.Deployments.Get("sample")
	if !ok {
		t.Fatalf("Deployment sample missing after apply")
	}
	if dep.GetAppliedRevision() != 1 {
		t.Errorf("Deployment.applied_revision = %d, want 1", dep.GetAppliedRevision())
	}
	if got := len(dep.GetServices()); got != 2 {
		t.Errorf("Deployment.services len = %d, want 2", got)
	}
	if got := c.A.State.Routes.Len(); got != 2 {
		t.Errorf("Routes.Len = %d, want 2", got)
	}
	if r, ok := c.A.State.Routes.Get("web.example.com"); !ok || r.GetTlsAuto() != true {
		t.Errorf("web.example.com route missing or tls_auto wrong: %+v", r)
	}
	if r, ok := c.A.State.Routes.Get("api.example.com"); !ok || r.GetTlsAuto() {
		t.Errorf("api.example.com route missing or tls_auto wrong: %+v", r)
	}
}

func TestDeploy_ApplyTwiceBumpsRevision(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	if _, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml: []byte(sampleJacoYAML), ComposeYaml: []byte(sampleComposeYAML),
	}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	resp, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml: []byte(sampleJacoYAML), ComposeYaml: []byte(sampleComposeYAML),
	})
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if got := resp.GetAppliedRevision(); got != 2 {
		t.Errorf("second applied_revision = %d, want 2", got)
	}
	dep, _ := c.A.State.Deployments.Get("sample")
	if dep.GetPreviousRevision() != 1 {
		t.Errorf("previous_revision = %d, want 1", dep.GetPreviousRevision())
	}
}

func TestDeploy_RollbackFlipsRevisions(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	deploy.Apply(ctx, &pb.ApplyRequest{JacoYaml: []byte(sampleJacoYAML), ComposeYaml: []byte(sampleComposeYAML)})
	deploy.Apply(ctx, &pb.ApplyRequest{JacoYaml: []byte(sampleJacoYAML), ComposeYaml: []byte(sampleComposeYAML)})

	resp, err := deploy.Rollback(ctx, &pb.RollbackRequest{Deployment: "sample"})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := resp.GetRevision(); got != 1 {
		t.Errorf("rollback revision = %d, want 1", got)
	}
	dep, _ := c.A.State.Deployments.Get("sample")
	if dep.GetAppliedRevision() != 1 || dep.GetPreviousRevision() != 2 {
		t.Errorf("after rollback: applied=%d previous=%d (want applied=1 previous=2)",
			dep.GetAppliedRevision(), dep.GetPreviousRevision())
	}
}

func TestDeploy_RollbackRefusesWhenNoPrevious(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	deploy.Apply(ctx, &pb.ApplyRequest{JacoYaml: []byte(sampleJacoYAML), ComposeYaml: []byte(sampleComposeYAML)})
	_, err := deploy.Rollback(ctx, &pb.RollbackRequest{Deployment: "sample"})
	if err == nil {
		t.Fatalf("expected no_previous_revision error")
	}
	sErr, _ := status.FromError(err)
	if sErr.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", sErr.Code())
	}
	if !strings.Contains(sErr.Message(), "no_previous_revision") {
		t.Errorf("message %q does not contain no_previous_revision", sErr.Message())
	}
}

func TestDeploy_DeleteCascadesRoutes(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	deploy.Apply(ctx, &pb.ApplyRequest{JacoYaml: []byte(sampleJacoYAML), ComposeYaml: []byte(sampleComposeYAML)})
	if c.A.State.Routes.Len() != 2 {
		t.Fatalf("preconditions: Routes.Len = %d, want 2", c.A.State.Routes.Len())
	}

	if _, err := deploy.Delete(ctx, &pb.DeleteRequest{Deployment: "sample"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := c.A.State.Deployments.Get("sample"); ok {
		t.Errorf("Deployment still present after Delete")
	}
	if c.A.State.Routes.Len() != 0 {
		t.Errorf("Routes.Len = %d, want 0 (cascade)", c.A.State.Routes.Len())
	}
}

func TestDeploy_ApplyRejectsUnknownComposeService(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	// name "ghost" doesn't exist as a service key in the compose file.
	bad := `deployment: sample
services:
  - name: ghost
    replicas: 1
`
	_, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(bad),
		ComposeYaml: []byte(sampleComposeYAML),
	})
	if err == nil {
		t.Fatalf("expected validation_failed")
	}
	sErr, _ := status.FromError(err)
	if !strings.Contains(sErr.Message(), "validation_failed") {
		t.Errorf("message = %q; want validation_failed", sErr.Message())
	}
}

func TestDeploy_ApplyRejectsComposeServiceField(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	// compose_service is no longer supported; should be rejected loudly.
	bad := `deployment: sample
services:
  - name: web
    replicas: 1
    compose_service: web
`
	_, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(bad),
		ComposeYaml: []byte(sampleComposeYAML),
	})
	if err == nil {
		t.Fatalf("expected error for compose_service field")
	}
	// The gRPC status code is "validation_failed"; the detailed message is
	// tested directly via TestParseJacoYAML_RejectsComposeServiceField.
	sErr, _ := status.FromError(err)
	if !strings.Contains(sErr.Message(), "validation_failed") {
		t.Errorf("message = %q; want 'validation_failed'", sErr.Message())
	}
}

func TestDeploy_ApplyRejectsUnknownComposeField(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	bad := `services:
  web:
    image: nginx
    deploy:
      replicas: 3
`
	_, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(sampleJacoYAML),
		ComposeYaml: []byte(bad),
	})
	if err == nil {
		t.Fatalf("expected validation error")
	}
	sErr, _ := status.FromError(err)
	if !strings.Contains(sErr.Message(), "validation_failed") {
		t.Errorf("message = %q; want validation_failed", sErr.Message())
	}
}

// --- helpers -----------------------------------------------------------------

func newDeployClient(t *testing.T, c *twoNodeCluster) pb.DeployClient {
	t.Helper()
	// Reuse the same conn the cluster setup built; build a DeployClient on it.
	// twoNodeCluster only exposes Tokens / Cluster / Audit so we open a fresh
	// connection here.
	conn := dialConn(t, c.A.Server.Addr().String(), c.A.CACert, "node-a")
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewDeployClient(conn)
}

// silence unused
var _ = context.Background
