package grpcsrv_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

const privilegedJacoYAML = `deployment: probe
services:
  - name: probe
    replicas: 1
`

// Compose body for a privileged service WITH the required label. The
// admission gate then turns on the calling token's allows_privileged flag.
const privilegedComposeYAML = `services:
  probe:
    image: nginx:1.27
    privileged: true
    security_opt:
      - seccomp=unconfined
    labels:
      jaco.io/allow-privileged: "true"
networks:
  default: {}
`

// Same shape as above but missing the gating label — the validator MUST
// reject this with InvalidArgument before admission ever sees the token.
const privilegedComposeNoLabelYAML = `services:
  probe:
    image: nginx:1.27
    privileged: true
networks:
  default: {}
`

// Vanilla compose body — no privileged/security_opt anywhere. Used to
// confirm the gate doesn't regress non-privileged applies.
const nonPrivilegedComposeYAML = `services:
  probe:
    image: nginx:1.27
networks:
  default: {}
`

// TestDeploy_ApplyAdmitsPrivilegedWithAllowFlag — issue #119 case 1: a
// manifest carrying `privileged:` / `security_opt:` plus the required label
// admits when the calling token is `allows_privileged=true`, and the
// per-service AUDIT_EVENT_TYPE_PRIVILEGED_WORKLOAD_ADMITTED event lands
// in state.AuditEvents with the deployment/service/identity/fields payload.
func TestDeploy_ApplyAdmitsPrivilegedWithAllowFlag(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)

	// Bootstrap token cannot grant privileged; issue a fresh one with
	// the flag and use it for the Apply.
	issueResp, err := c.A.Tokens.Issue(authContext(c.OperatorToken), &pb.TokenIssueRequest{
		Identity:         "ops",
		AllowsPrivileged: true,
	})
	if err != nil {
		t.Fatalf("Issue ops token: %v", err)
	}

	resp, err := deploy.Apply(authContext(issueResp.GetToken()), &pb.ApplyRequest{
		JacoYaml:    []byte(privilegedJacoYAML),
		ComposeYaml: []byte(privilegedComposeYAML),
	})
	if err != nil {
		t.Fatalf("Apply: unexpected err: %v", err)
	}
	if resp.GetAppliedRevision() == 0 {
		t.Errorf("AppliedRevision = 0, want >= 1")
	}

	// AuditAppend lands via raft; allow the local apply a brief window
	// to drain before asserting on state.
	deadline := time.Now().Add(2 * time.Second)
	var admitted *pb.AuditEvent
	for time.Now().Before(deadline) {
		for _, ev := range c.A.State.AuditEvents.List() {
			if ev.GetType() == pb.AuditEventType_AUDIT_EVENT_TYPE_PRIVILEGED_WORKLOAD_ADMITTED {
				admitted = ev
				break
			}
		}
		if admitted != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if admitted == nil {
		t.Fatalf("no AUDIT_EVENT_TYPE_PRIVILEGED_WORKLOAD_ADMITTED event found")
	}
	if admitted.GetPayload()["deployment"] != "probe" {
		t.Errorf("payload[deployment] = %q, want probe", admitted.GetPayload()["deployment"])
	}
	if admitted.GetPayload()["service"] != "probe" {
		t.Errorf("payload[service] = %q, want probe", admitted.GetPayload()["service"])
	}
	if admitted.GetPayload()["identity"] != "ops" {
		t.Errorf("payload[identity] = %q, want ops", admitted.GetPayload()["identity"])
	}
	if got := admitted.GetPayload()["fields"]; got != "privileged,security_opt" {
		t.Errorf("payload[fields] = %q, want privileged,security_opt", got)
	}
}

// TestDeploy_ApplyRejectsPrivilegedWithoutAllowFlag — issue #119 case 2:
// the same manifest as case 1, but the calling token (bootstrap) is
// `allows_privileged=false`, MUST be rejected with PermissionDenied
// whose detail message names the offending service and the gated fields.
func TestDeploy_ApplyRejectsPrivilegedWithoutAllowFlag(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)

	_, err := deploy.Apply(authContext(c.OperatorToken), &pb.ApplyRequest{
		JacoYaml:    []byte(privilegedJacoYAML),
		ComposeYaml: []byte(privilegedComposeYAML),
	})
	if err == nil {
		t.Fatalf("Apply: want PermissionDenied, got nil")
	}
	sErr, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err is not a status: %T %v", err, err)
	}
	if sErr.Code() != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", sErr.Code())
	}
	if sErr.Message() != "privilege_denied" {
		t.Errorf("status message = %q, want privilege_denied", sErr.Message())
	}
	if !errorDetailContains(t, err, "probe") {
		t.Errorf("error detail does not mention service probe: %v", err)
	}
	if !errorDetailContains(t, err, "allows_privileged") {
		t.Errorf("error detail does not mention allows_privileged: %v", err)
	}

	// And no admission audit event should have been emitted.
	for _, ev := range c.A.State.AuditEvents.List() {
		if ev.GetType() == pb.AuditEventType_AUDIT_EVENT_TYPE_PRIVILEGED_WORKLOAD_ADMITTED {
			t.Errorf("PRIVILEGED_WORKLOAD_ADMITTED audit emitted on rejected apply: %+v", ev)
		}
	}
}

// TestDeploy_ApplyRejectsPrivilegedWithoutLabel — issue #119 case 3: a
// privileged manifest that omits `jaco.io/allow-privileged: "true"` is
// rejected at the validator with InvalidArgument; even an allows_privileged
// token cannot bypass the manifest-level marker.
func TestDeploy_ApplyRejectsPrivilegedWithoutLabel(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)

	// Use an allows_privileged token to prove this rejection happens
	// at the validator (label missing), not at admission.
	issueResp, err := c.A.Tokens.Issue(authContext(c.OperatorToken), &pb.TokenIssueRequest{
		Identity:         "ops",
		AllowsPrivileged: true,
	})
	if err != nil {
		t.Fatalf("Issue ops token: %v", err)
	}

	_, err = deploy.Apply(authContext(issueResp.GetToken()), &pb.ApplyRequest{
		JacoYaml:    []byte(privilegedJacoYAML),
		ComposeYaml: []byte(privilegedComposeNoLabelYAML),
	})
	if err == nil {
		t.Fatalf("Apply: want InvalidArgument, got nil")
	}
	sErr, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err is not a status: %T %v", err, err)
	}
	if sErr.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", sErr.Code())
	}
	if !strings.Contains(sErr.Message(), "validation_failed") {
		t.Errorf("status message = %q, want to mention validation_failed", sErr.Message())
	}
	if !errorDetailContains(t, err, "jaco.io/allow-privileged") {
		t.Errorf("error detail does not mention jaco.io/allow-privileged: %v", err)
	}
}

// TestDeploy_ApplyNonPrivilegedNoFlagRegression — issue #119 case 4: a
// manifest that sets neither `privileged:` nor `security_opt:` MUST admit
// under the bootstrap token (allows_privileged=false). The gate must not
// regress any non-privileged apply path.
func TestDeploy_ApplyNonPrivilegedNoFlagRegression(t *testing.T) {
	c := setupTwoNodeCluster(t)
	deploy := newDeployClient(t, c)

	resp, err := deploy.Apply(authContext(c.OperatorToken), &pb.ApplyRequest{
		JacoYaml:    []byte(privilegedJacoYAML),
		ComposeYaml: []byte(nonPrivilegedComposeYAML),
	})
	if err != nil {
		t.Fatalf("Apply: unexpected err: %v", err)
	}
	if resp.GetAppliedRevision() == 0 {
		t.Errorf("AppliedRevision = 0, want >= 1")
	}
	// No admission audit should land on a non-privileged apply.
	for _, ev := range c.A.State.AuditEvents.List() {
		if ev.GetType() == pb.AuditEventType_AUDIT_EVENT_TYPE_PRIVILEGED_WORKLOAD_ADMITTED {
			t.Errorf("unexpected privileged-admit audit on non-privileged apply: %+v", ev)
		}
	}
}
