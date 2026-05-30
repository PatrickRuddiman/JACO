package grpcsrv_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
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
	if r, ok := c.A.State.Routes.Get(state.RouteKey("web.example.com", "")); !ok || r.GetTlsAuto() != true {
		t.Errorf("web.example.com route missing or tls_auto wrong: %+v", r)
	}
	if r, ok := c.A.State.Routes.Get(state.RouteKey("api.example.com", "")); !ok || r.GetTlsAuto() {
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

// TCP-ingress fixtures: deployment "data" (service db) and "cache" (service
// redis) both want host port 5432 — used by the collision tests.
const dbJacoYAML = `deployment: data
services:
  - name: db
    replicas: 1
`

const dbComposeYAML = `services:
  db:
    image: postgres:16
    ports:
      - "5432:5432"
`

const cacheJacoYAML = `deployment: cache
services:
  - name: redis
    replicas: 1
`

const cacheComposeYAML = `services:
  redis:
    image: redis:7
    ports:
      - "5432:5432"
`

func TestDeploy_ApplyDerivesTCPRoutes(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	if _, err := deploy.Apply(ctx, &pb.ApplyRequest{JacoYaml: []byte(dbJacoYAML), ComposeYaml: []byte(dbComposeYAML)}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	r, ok := c.A.State.TCPRoutes.Get(state.TCPRouteKey(5432))
	if !ok {
		t.Fatalf("TCPRoute 5432 missing after apply")
	}
	if r.GetDeployment() != "data" || r.GetService() != "db" || r.GetContainerPort() != 5432 {
		t.Errorf("unexpected TCPRoute: %+v", r)
	}
}

func TestDeploy_ApplyRejectsTCPPortConflict(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	if _, err := deploy.Apply(ctx, &pb.ApplyRequest{JacoYaml: []byte(dbJacoYAML), ComposeYaml: []byte(dbComposeYAML)}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	// A different deployment publishing the same host port must be rejected.
	_, err := deploy.Apply(ctx, &pb.ApplyRequest{JacoYaml: []byte(cacheJacoYAML), ComposeYaml: []byte(cacheComposeYAML)})
	if err == nil {
		t.Fatalf("expected port_conflict, got nil")
	}
	sErr, _ := status.FromError(err)
	if sErr.Code() != codes.InvalidArgument || !strings.Contains(sErr.Message(), "port_conflict") {
		t.Errorf("err = %v (code %v); want InvalidArgument/port_conflict", sErr.Message(), sErr.Code())
	}
	// The conflicting deployment must not have been created.
	if _, ok := c.A.State.Deployments.Get("cache"); ok {
		t.Errorf("conflicting deployment cache was created despite port_conflict")
	}
}

func TestDeploy_ReapplySamePortNoConflict(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	if _, err := deploy.Apply(ctx, &pb.ApplyRequest{JacoYaml: []byte(dbJacoYAML), ComposeYaml: []byte(dbComposeYAML)}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	// Re-applying the same deployment reclaiming its own port is not a conflict.
	if _, err := deploy.Apply(ctx, &pb.ApplyRequest{JacoYaml: []byte(dbJacoYAML), ComposeYaml: []byte(dbComposeYAML)}); err != nil {
		t.Fatalf("re-apply same deployment: unexpected error: %v", err)
	}
}

// --- Issue #99: slim jaco.yaml + compose-as-source-of-truth ------------------

// TestDeploy_ApplyEmptyServicesValidatesWhenRoutesValid — issue #99: a
// jaco.yaml that omits `services:` entirely is valid as long as the routes
// resolve against the compose service set. The compose web service supplies
// the default ServiceSpec.
func TestDeploy_ApplyEmptyServicesValidatesWhenRoutesValid(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	const slimJaco = `deployment: slim
routes:
  - domain: slim.example.com
    service: web
    port: 80
    tls: auto
`
	const composeYAML = `services:
  web:
    image: nginx:1.27
`
	resp, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(slimJaco),
		ComposeYaml: []byte(composeYAML),
	})
	if err != nil {
		t.Fatalf("Apply with empty services rejected: %v", err)
	}
	if resp.GetAppliedRevision() != 1 {
		t.Errorf("applied_revision = %d, want 1", resp.GetAppliedRevision())
	}
	dep, ok := c.A.State.Deployments.Get("slim")
	if !ok {
		t.Fatalf("Deployment slim missing after apply")
	}
	specs := dep.GetServices()
	if len(specs) != 1 || specs[0].GetName() != "web" {
		t.Fatalf("Deployment.services = %+v, want [web]", specs)
	}
	if specs[0].GetReplicas() != 1 {
		t.Errorf("web Replicas = %d, want 1 (default for compose-only)", specs[0].GetReplicas())
	}
	if specs[0].GetPlacement() != pb.ServiceSpec_PLACEMENT_MODE_SPREAD {
		t.Errorf("web Placement = %v, want SPREAD", specs[0].GetPlacement())
	}
}

// TestDeploy_ApplyComposeReplicasFlowThrough — issue #99 acceptance: when
// JACO omits replicas, compose `deploy.replicas: 3` lands on
// ServiceSpec.Replicas.
func TestDeploy_ApplyComposeReplicasFlowThrough(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	const jacoYAML = `deployment: scaled
services:
  - name: web
    placement: spread
routes:
  - domain: scaled.example.com
    service: web
    port: 80
    tls: auto
`
	const composeYAML = `services:
  web:
    image: nginx:1.27
    deploy:
      replicas: 3
`
	if _, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(jacoYAML),
		ComposeYaml: []byte(composeYAML),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dep, ok := c.A.State.Deployments.Get("scaled")
	if !ok {
		t.Fatalf("Deployment scaled missing after apply")
	}
	specs := dep.GetServices()
	if len(specs) != 1 {
		t.Fatalf("services len = %d, want 1", len(specs))
	}
	if got := specs[0].GetReplicas(); got != 3 {
		t.Errorf("Replicas = %d, want 3 (from compose deploy.replicas)", got)
	}
}

// TestDeploy_ApplyComposeOnlyServiceProducesDefault — issue #99 acceptance:
// a compose service with no JACO entry still produces a ServiceSpec with
// placement=spread and replicas=1.
func TestDeploy_ApplyComposeOnlyServiceProducesDefault(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	const jacoYAML = `deployment: mixed
services:
  - name: web
    replicas: 2
routes:
  - domain: mixed.example.com
    service: web
    port: 80
    tls: auto
`
	const composeYAML = `services:
  web:
    image: nginx:1.27
  worker:
    image: busybox:1.36
`
	if _, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(jacoYAML),
		ComposeYaml: []byte(composeYAML),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dep, _ := c.A.State.Deployments.Get("mixed")
	specs := dep.GetServices()
	if len(specs) != 2 {
		t.Fatalf("services len = %d, want 2 (web + worker)", len(specs))
	}
	// Specs are sorted by name.
	if specs[0].GetName() != "web" || specs[0].GetReplicas() != 2 {
		t.Errorf("web spec = %+v, want name=web replicas=2", specs[0])
	}
	if specs[1].GetName() != "worker" || specs[1].GetReplicas() != 1 ||
		specs[1].GetPlacement() != pb.ServiceSpec_PLACEMENT_MODE_SPREAD {
		t.Errorf("worker spec = %+v, want name=worker replicas=1 placement=SPREAD", specs[1])
	}
}

// TestDeploy_ApplyRejectsGlobalWithReplicas — issue #99: `placement: global`
// runs one replica per ready node, so an explicit `replicas:` is mutually
// exclusive and rejected at Apply.
func TestDeploy_ApplyRejectsGlobalWithReplicas(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	const jacoYAML = `deployment: agent
services:
  - name: probe
    placement: global
    replicas: 3
`
	const composeYAML = `services:
  probe:
    image: busybox:1.36
`
	_, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(jacoYAML),
		ComposeYaml: []byte(composeYAML),
	})
	if err == nil {
		t.Fatalf("expected rejection for placement=global + explicit replicas")
	}
	sErr, _ := status.FromError(err)
	if sErr.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", sErr.Code())
	}
	if !strings.Contains(sErr.Message(), "validation_failed") {
		t.Errorf("message = %q; want substring 'validation_failed'", sErr.Message())
	}
	if !errorDetailContains(t, err, "placement=global") {
		t.Errorf("error details missing 'placement=global'; full err = %v", err)
	}
}

// TestDeploy_ApplyRouteReferencesComposeOnlyService — issue #99: a route
// may target a service that has no JACO `services:` entry as long as the
// compose project declares it.
func TestDeploy_ApplyRouteReferencesComposeOnlyService(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	const jacoYAML = `deployment: routed
routes:
  - domain: routed.example.com
    service: web
    port: 80
    tls: auto
`
	const composeYAML = `services:
  web:
    image: nginx:1.27
`
	if _, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(jacoYAML),
		ComposeYaml: []byte(composeYAML),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := c.A.State.Routes.Get(state.RouteKey("routed.example.com", "")); !ok {
		t.Errorf("routed.example.com route missing after apply")
	}
}

// TestDeploy_ApplyRejectsRouteReferencingUnknownService — a route whose
// service field doesn't match ANY entry (neither JACO nor compose) is still
// rejected. The check now runs at Apply against the merged set; the error
// surface stays the same.
func TestDeploy_ApplyRejectsRouteReferencingUnknownService(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	const jacoYAML = `deployment: bad
routes:
  - domain: bad.example.com
    service: ghost
    port: 80
    tls: auto
`
	const composeYAML = `services:
  web:
    image: nginx:1.27
`
	_, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(jacoYAML),
		ComposeYaml: []byte(composeYAML),
	})
	if err == nil {
		t.Fatalf("expected rejection for route → unknown service")
	}
	sErr, _ := status.FromError(err)
	if sErr.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", sErr.Code())
	}
	if !errorDetailContains(t, err, "unknown service") {
		t.Errorf("error details missing 'unknown service'; full err = %v", err)
	}
}

// TestDeploy_ApplyJacoNetworksOverrideCompose — when both JACO and compose
// declare networks, JACO wins; when only compose declares them, compose's
// list flows through. End-to-end version of the MergeServiceDefaults unit.
func TestDeploy_ApplyJacoNetworksOverrideCompose(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)
	ctx := authContext(c.OperatorToken)

	const jacoYAML = `deployment: netmix
services:
  - name: web
    networks: [backend]
`
	const composeYAML = `services:
  web:
    image: nginx:1.27
    networks: [frontend]
  gateway:
    image: nginx:1.27
    networks: [frontend, backend]
networks:
  frontend: {}
  backend: {}
`
	if _, err := deploy.Apply(ctx, &pb.ApplyRequest{
		JacoYaml:    []byte(jacoYAML),
		ComposeYaml: []byte(composeYAML),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dep, _ := c.A.State.Deployments.Get("netmix")
	var web, gateway *pb.ServiceSpec
	for _, s := range dep.GetServices() {
		switch s.GetName() {
		case "web":
			web = s
		case "gateway":
			gateway = s
		}
	}
	if web == nil || gateway == nil {
		t.Fatalf("missing services in deployment: %+v", dep.GetServices())
	}
	// JACO override wins: web on backend only.
	if got, want := web.GetNetworks(), []string{"backend"}; !equalDeployStrings(got, want) {
		t.Errorf("web Networks = %v, want %v (JACO override)", got, want)
	}
	// Compose default flows through: gateway on alphabetically sorted backend+frontend.
	if got, want := gateway.GetNetworks(), []string{"backend", "frontend"}; !equalDeployStrings(got, want) {
		t.Errorf("gateway Networks = %v, want %v (compose default)", got, want)
	}
}

// --- end issue #99 -----------------------------------------------------------

func equalDeployStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// errorDetailContains reports whether the pb.Error riding in the gRPC
// status details contains substr. errorStatus embeds the human message in
// the typed Error proto, not in the status' top-level message (which only
// carries the stable code like "validation_failed"); tests that need to
// assert on the message text MUST read it from details.
func errorDetailContains(t *testing.T, err error, substr string) bool {
	t.Helper()
	sErr, _ := status.FromError(err)
	for _, d := range sErr.Details() {
		if e, ok := d.(*pb.Error); ok && strings.Contains(e.GetMessage(), substr) {
			return true
		}
	}
	return false
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
