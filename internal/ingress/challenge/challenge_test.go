package challenge_test

import (
	"errors"
	"testing"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/ingress/challenge"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newHarness(t *testing.T) (challenge.Applier, *state.State) {
	t.Helper()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var idx uint64
	apply := func(data []byte) error {
		idx++
		f.Apply(&hraft.Log{Index: idx, Data: data})
		return nil
	}
	return apply, st
}

func TestClassifyFailure(t *testing.T) {
	cases := map[string]challenge.FailureClass{
		"urn:ietf:params:acme:error:rateLimited: too many certificates": challenge.FailureRateLimit,
		"acme: error: 429 rate limit exceeded":                          challenge.FailureRateLimit,
		"no such host for challenge domain":                             challenge.FailureValidation,
		"dns problem: NXDOMAIN looking up A":                            challenge.FailureValidation,
		"unauthorized: incorrect validation certificate":                challenge.FailureValidation,
		"503 service unavailable":                                       challenge.FailureTransient,
		"something totally unexpected":                                  challenge.FailureUnknown,
	}
	for msg, want := range cases {
		if got := challenge.ClassifyFailure(errors.New(msg)); got != want {
			t.Errorf("ClassifyFailure(%q) = %s, want %s", msg, got, want)
		}
	}
	if got := challenge.ClassifyFailure(nil); got != challenge.FailureUnknown {
		t.Errorf("ClassifyFailure(nil) = %s, want unknown", got)
	}
}

func TestEmitStageFailure_AuditFields(t *testing.T) {
	apply, st := newHarness(t)
	iss := challenge.NewIssuer(apply)
	iss.EmitStageFailure("dns-broken.example.com", errors.New("dns problem: NXDOMAIN"))

	var ev *pb.AuditEvent
	for _, e := range st.AuditEvents.List() {
		if e.GetType() == pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_FAILED {
			ev = e
		}
	}
	if ev == nil {
		t.Fatalf("no CERTIFICATE_FAILED event emitted")
	}
	p := ev.GetPayload()
	if p["stage_failed_at"] != "staging" {
		t.Errorf("stage_failed_at = %q, want staging", p["stage_failed_at"])
	}
	if p["acme_environment"] != challenge.EnvStaging {
		t.Errorf("acme_environment = %q, want staging", p["acme_environment"])
	}
	if p["failure_class"] != string(challenge.FailureValidation) {
		t.Errorf("failure_class = %q, want validation", p["failure_class"])
	}
	if p["domain"] != "dns-broken.example.com" {
		t.Errorf("domain = %q", p["domain"])
	}
}

func TestEmitIssued_TagsEnvironment(t *testing.T) {
	apply, st := newHarness(t)
	challenge.NewIssuerForEnv(apply, challenge.EnvProd).EmitIssued("web.example.com", challenge.EnvProd)

	var ev *pb.AuditEvent
	for _, e := range st.AuditEvents.List() {
		if e.GetType() == pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_ISSUED {
			ev = e
		}
	}
	if ev == nil {
		t.Fatalf("no CERTIFICATE_ISSUED event emitted")
	}
	if ev.GetPayload()["acme_environment"] != challenge.EnvProd {
		t.Errorf("acme_environment = %q, want prod", ev.GetPayload()["acme_environment"])
	}
}

func TestIssue_StampsEnvironmentOnRenewed(t *testing.T) {
	apply, st := newHarness(t)
	iss := challenge.NewIssuerForEnv(apply, challenge.EnvStaging)
	if err := iss.Issue(nil, "x.example.com", "tok", "keyauth"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	var ev *pb.AuditEvent
	for _, e := range st.AuditEvents.List() {
		if e.GetType() == pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED {
			ev = e
		}
	}
	if ev == nil {
		t.Fatalf("no CERTIFICATE_RENEWED event")
	}
	if ev.GetPayload()["acme_environment"] != challenge.EnvStaging {
		t.Errorf("acme_environment = %q, want staging", ev.GetPayload()["acme_environment"])
	}
}
