//go:build acme

package ingress_test

import (
	"context"
	"os"
	"testing"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/ingress/challenge"
	"github.com/PatrickRuddiman/jaco/internal/ingress/storage"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestIntegration_AcmeIssuanceAgainstPebble drives the full ACME issuance
// path against a running Pebble instance pointed to by JACO_INTEGRATION_
// PEBBLE (e.g. https://pebble:14000/dir). Skipped when the env var is
// unset.
//
// v0 scope: asserts the Issuer side persists ChallengeToken + audit
// emission. Full cert issuance via certmagic needs the embedded caddy
// import which isn't wired in v0 (the daemon execs an external caddy);
// the cert-handed-to-storage step is exercised by the storage tests in
// task 40.
func TestIntegration_AcmeIssuanceAgainstPebble(t *testing.T) {
	if os.Getenv("JACO_INTEGRATION_PEBBLE") == "" {
		t.Skip("set JACO_INTEGRATION_PEBBLE=<dir-url> to enable")
	}

	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var raftIdx uint64
	applier := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}

	issuer := challenge.NewIssuer(applier, nil)
	if err := issuer.Issue(context.Background(), "test.jaco.local", "tok-1", "key-auth-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, ok := st.ChallengeTokens.Get("tok-1"); !ok {
		t.Fatalf("ChallengeToken tok-1 missing from state after Issue")
	}

	// Audit emission landed in state.AuditEvents.
	var sawRenewed bool
	for _, ev := range st.AuditEvents.List() {
		if ev.GetType() == pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED {
			sawRenewed = true
			break
		}
	}
	if !sawRenewed {
		t.Errorf("no CERTIFICATE_RENEWED audit event after successful Issue")
	}

	// Smoke-test that a CertBlob round-trips via the storage layer.
	store := storage.New(st, applier, "test-node", nil)
	if err := store.Store(context.Background(), "certificates/test.jaco.local/cert.pem", []byte("pem-bytes")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := store.Load(context.Background(), "certificates/test.jaco.local/cert.pem")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != "pem-bytes" {
		t.Errorf("Load = %q, want pem-bytes", got)
	}

	_ = proto.Marshal
}
