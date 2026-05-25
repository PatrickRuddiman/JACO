package grpc

import (
	"bytes"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestReplicaStateString_AllVariants — the enum-to-string adapter feeds
// the ingress builder's ReplicaObservedView.State field. Every branch
// matters because Caddy decides liveness by string compare.
func TestReplicaStateString_AllVariants(t *testing.T) {
	cases := []struct {
		in   pb.ReplicaState
		want string
	}{
		{pb.ReplicaState_REPLICA_STATE_RUNNING, "running"},
		{pb.ReplicaState_REPLICA_STATE_DEGRADED, "degraded"},
		{pb.ReplicaState_REPLICA_STATE_FAILED, "failed"},
		{pb.ReplicaState_REPLICA_STATE_PENDING, "pending"},
		{pb.ReplicaState_REPLICA_STATE_UNSPECIFIED, ""},
		{pb.ReplicaState_REPLICA_STATE_PULLING, ""},
		{pb.ReplicaState_REPLICA_STATE_UPDATING, ""},
		{pb.ReplicaState_REPLICA_STATE_STOPPED, ""},
	}
	for _, c := range cases {
		if got := replicaStateString(c.in); got != c.want {
			t.Errorf("replicaStateString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReplicaIDDeployment_AndService — both look up the ReplicaDesired
// entry by id; missing id returns empty string (used as a soft-skip in
// the ingress builder).
func TestReplicaIDDeployment_AndService(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{
		Id: "smoke-web-0", Deployment: "smoke", Service: "web", Host: "h1",
	}, 1)

	if got := replicaIDDeployment("smoke-web-0", st); got != "smoke" {
		t.Errorf("replicaIDDeployment hit = %q, want smoke", got)
	}
	if got := replicaIDDeployment("missing", st); got != "" {
		t.Errorf("replicaIDDeployment miss = %q, want \"\"", got)
	}
	if got := replicaIDService("smoke-web-0", st); got != "web" {
		t.Errorf("replicaIDService hit = %q, want web", got)
	}
	if got := replicaIDService("missing", st); got != "" {
		t.Errorf("replicaIDService miss = %q, want \"\"", got)
	}
}

// TestAuditTypeFromString_AllKnownCodes — the firewall reconciler emits
// these strings; the daemon translates to the pb enum for AuditEvents.
func TestAuditTypeFromString_AllKnownCodes(t *testing.T) {
	cases := []struct {
		in   string
		want pb.AuditEventType
	}{
		{"ISOLATION_RULESET_RECONCILED", pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_RULESET_RECONCILED},
		{"ISOLATION_UNAVAILABLE", pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_UNAVAILABLE},
		// Unknown falls through to RECONCILED (documented behaviour in
		// server.go).
		{"", pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_RULESET_RECONCILED},
		{"some_other_code", pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_RULESET_RECONCILED},
	}
	for _, c := range cases {
		if got := auditTypeFromString(c.in); got != c.want {
			t.Errorf("auditTypeFromString(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestNodeStatusFromString — the firewall reconciler's status strings
// map to the pb.NodeStatus enum the FSM stores.
func TestNodeStatusFromString(t *testing.T) {
	cases := []struct {
		in   string
		want pb.NodeStatus
	}{
		{"ready", pb.NodeStatus_NODE_STATUS_READY},
		{"isolation_unavailable", pb.NodeStatus_NODE_STATUS_ISOLATION_UNAVAILABLE},
		{"", pb.NodeStatus_NODE_STATUS_READY},
		{"unknown", pb.NodeStatus_NODE_STATUS_READY},
	}
	for _, c := range cases {
		if got := nodeStatusFromString(c.in); got != c.want {
			t.Errorf("nodeStatusFromString(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestCaddyAvailable_ExecMode — JACO_INGRESS_EXEC=1 falls back to
// "caddy on PATH"; with PATH cleared the lookup must return false.
// Embedded mode (default) always returns true because the caddy/v2
// import statically links the runtime.
func TestCaddyAvailable_ExecMode(t *testing.T) {
	t.Setenv("JACO_INGRESS_EXEC", "1")
	t.Setenv("PATH", "")
	if caddyAvailable() {
		t.Errorf("caddyAvailable() = true with empty PATH and JACO_INGRESS_EXEC=1")
	}
}

func TestCaddyAvailable_EmbeddedMode(t *testing.T) {
	t.Setenv("JACO_INGRESS_EXEC", "")
	if !caddyAvailable() {
		t.Errorf("caddyAvailable() = false in embedded mode; caddy/v2 is statically linked")
	}
}

// TestIsRaftExistsErr — the daemon switches on this to surface
// FailedPrecondition vs Internal. nil err is false; matching substring
// is true; unrelated err is false.
func TestIsRaftExistsErr(t *testing.T) {
	if isRaftExistsErr(nil) {
		t.Errorf("isRaftExistsErr(nil) = true")
	}
	if !isRaftExistsErr(errExistsForTest{}) {
		t.Errorf("isRaftExistsErr(matching) = false")
	}
	if isRaftExistsErr(errOtherForTest{}) {
		t.Errorf("isRaftExistsErr(unrelated) = true")
	}
}

type errExistsForTest struct{}

func (errExistsForTest) Error() string { return "wrap: raft state already exists in /tmp/x" }

type errOtherForTest struct{}

func (errOtherForTest) Error() string { return "something else" }

// TestRaftExists_TempDirs — empty dataDir returns false; missing log.db
// returns false; existing log.db returns true.
func TestRaftExists_TempDirs(t *testing.T) {
	if raftExists("") {
		t.Errorf("raftExists(empty) = true")
	}
	if raftExists(t.TempDir()) {
		t.Errorf("raftExists(empty tempdir) = true")
	}
}

// TestLeaderGRPCAddr_NilGuards — empty inputs return "".
func TestLeaderGRPCAddr_NilGuards(t *testing.T) {
	if got := leaderGRPCAddr(nil, nil); got != "" {
		t.Errorf("leaderGRPCAddr(nil, nil) = %q, want \"\"", got)
	}
}

// TestIngressBuilder_ACMEDisabledOmitsAutomation — with the cluster-wide
// opt-out (acme_enabled: false), the builder renders no tls.automation block
// even when a tls:auto route exists (issue #41). Verifiable offline.
func TestIngressBuilder_ACMEDisabledOmitsAutomation(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Routes.Apply(&pb.Route{Domain: "web.example.com", Deployment: "s", Service: "web", Port: 80, TlsAuto: true}, 1)

	build := ingressBuilder(st, ingressACMEOpts{Email: "ops@x.com", CA: "https://acme-v02.api.letsencrypt.org/directory", Enabled: false})
	cfg, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if bytes.Contains(cfg, []byte("automation")) || bytes.Contains(cfg, []byte("letsencrypt")) || bytes.Contains(cfg, []byte("acme")) {
		t.Errorf("acme_enabled=false rendered an automation/acme block:\n%s", cfg)
	}
}

// TestIngressBuilder_PlumbsEmailAndCA — acme_email + acme_ca reach the
// rendered issuer (the plumbing-gap fix).
func TestIngressBuilder_PlumbsEmailAndCA(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Routes.Apply(&pb.Route{Domain: "web.example.com", Deployment: "s", Service: "web", Port: 80, TlsAuto: true}, 1)

	build := ingressBuilder(st, ingressACMEOpts{
		Email:   "ops@example.com",
		CA:      "https://acme-staging-v02.api.letsencrypt.org/directory",
		Enabled: true,
	})
	cfg, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !bytes.Contains(cfg, []byte("ops@example.com")) {
		t.Errorf("acme_email not plumbed into rendered config:\n%s", cfg)
	}
	if !bytes.Contains(cfg, []byte("acme-staging-v02")) {
		t.Errorf("acme_ca not plumbed into rendered config:\n%s", cfg)
	}
}
