package grpcsrv_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func leafPEM(t *testing.T, san string, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: san},
		DNSNames:     []string{san},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestStatus_ReportsCertState — issue #41: jaco status surfaces per-domain
// cert state derived from the cert blob (not_after) + the latest cert audit
// event (environment + last_renewal_at).
func TestStatus_ReportsCertState(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var idx uint64
	apply := func(cmd *pb.Command) {
		idx++
		data, err := proto.Marshal(cmd)
		if err != nil {
			t.Fatal(err)
		}
		f.Apply(&hraft.Log{Index: idx, Data: data})
	}

	// A tls:auto route for the domain.
	apply(&pb.Command{Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
		Deployment: "sample", Revision: 1,
		Routes: []*pb.Route{{Domain: "web.example.com", Deployment: "sample", Service: "web", Port: 80, TlsAuto: true}},
	}}})

	// The issued leaf cert blob (drives not_after).
	notAfter := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	apply(&pb.Command{Payload: &pb.Command_CertBlobUpsert{CertBlobUpsert: &pb.CertBlobUpsert{
		Blob: &pb.CertBlob{
			Key:   "certificates/acme-v02.api.letsencrypt.org-directory/web.example.com/web.example.com.crt",
			Value: leafPEM(t, "web.example.com", notAfter),
		},
	}}})

	// The cert audit event (drives environment + last_renewal_at).
	renewedAt := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	apply(&pb.Command{Ts: timestamppb.New(renewedAt), Payload: &pb.Command_AuditAppend{AuditAppend: &pb.AuditAppend{
		Event: &pb.AuditEvent{
			Type:    pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_ISSUED,
			Ts:      timestamppb.New(renewedAt),
			Payload: map[string]string{"domain": "web.example.com", "acme_environment": "prod"},
		},
	}}})

	srv := grpcsrv.NewDeployServer(st, nil)
	resp, err := srv.Status(context.Background(), &pb.DeployStatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(resp.GetCerts()) != 1 {
		t.Fatalf("certs = %d, want 1: %+v", len(resp.GetCerts()), resp.GetCerts())
	}
	cs := resp.GetCerts()[0]
	if cs.GetDomain() != "web.example.com" {
		t.Errorf("domain = %q", cs.GetDomain())
	}
	if cs.GetEnvironment() != "prod" {
		t.Errorf("environment = %q, want prod", cs.GetEnvironment())
	}
	if !cs.GetNotAfter().AsTime().Equal(notAfter) {
		t.Errorf("not_after = %v, want %v", cs.GetNotAfter().AsTime(), notAfter)
	}
	if !cs.GetLastRenewalAt().AsTime().Equal(renewedAt) {
		t.Errorf("last_renewal_at = %v, want %v", cs.GetLastRenewalAt().AsTime(), renewedAt)
	}
}

// TestStatus_NoCertStateForTLSOffRoutes — a tls:off route never reports cert
// state even if a stray blob exists.
func TestStatus_NoCertStateForTLSOffRoutes(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var idx uint64
	apply := func(cmd *pb.Command) {
		idx++
		data, _ := proto.Marshal(cmd)
		f.Apply(&hraft.Log{Index: idx, Data: data})
	}
	apply(&pb.Command{Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
		Deployment: "sample", Revision: 1,
		Routes: []*pb.Route{{Domain: "plain.example.com", Deployment: "sample", Service: "web", Port: 80, TlsAuto: false}},
	}}})

	srv := grpcsrv.NewDeployServer(st, nil)
	resp, err := srv.Status(context.Background(), &pb.DeployStatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(resp.GetCerts()) != 0 {
		t.Errorf("certs = %d, want 0 for tls:off route", len(resp.GetCerts()))
	}
}
