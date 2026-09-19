package seal_test

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	"github.com/PatrickRuddiman/jaco/internal/testutil"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func TestJoinProofBindsBothSidesAndKeyring(t *testing.T) {
	keys := testutil.StateKeys(t)
	req := &pb.NodeJoinRequest{
		Name: "node-b", JoinToken: "synthetic-single-use-token", CsrPem: []byte("synthetic-CSR"),
		AdvertiseAddr: "127.0.0.1:7000", GrpcAddress: "127.0.0.1:7443", WireguardPubkey: []byte("public-key"),
	}
	if err := keys.VerifyJoinRequest(req); err == nil {
		t.Fatal("missing proof accepted")
	}
	if err := keys.SignJoinRequest(req); err != nil {
		t.Fatal(err)
	}
	if err := keys.VerifyJoinRequest(req); err != nil {
		t.Fatal(err)
	}
	wrong, err := seal.New("test-only", map[string][]byte{"test-only": bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.VerifyJoinRequest(req); err == nil {
		t.Fatal("keyring mismatch accepted")
	}
	changes := []func(*pb.NodeJoinRequest){
		func(r *pb.NodeJoinRequest) { r.Name = "node-c" },
		func(r *pb.NodeJoinRequest) { r.JoinToken += "-replay" },
		func(r *pb.NodeJoinRequest) { r.CsrPem = []byte("other-CSR") },
		func(r *pb.NodeJoinRequest) { r.AdvertiseAddr = "127.0.0.2:7000" },
		func(r *pb.NodeJoinRequest) { r.GrpcAddress = "127.0.0.2:7443" },
		func(r *pb.NodeJoinRequest) { r.WireguardPubkey = []byte("other-key") },
	}
	for i, change := range changes {
		altered := proto.Clone(req).(*pb.NodeJoinRequest)
		change(altered)
		if err := keys.VerifyJoinRequest(altered); err == nil {
			t.Errorf("request mutation %d accepted", i)
		}
	}
	resp := &pb.NodeJoinResponse{
		ClusterId: "cluster-a", SignedCert: []byte("node-cert"), CaCert: []byte("CA"),
		PeerAddrs: []string{"127.0.0.1:7000"},
	}
	if err := keys.SignJoinResponse(req, resp); err != nil {
		t.Fatal(err)
	}
	if err := keys.VerifyJoinResponse(req, resp); err != nil {
		t.Fatal(err)
	}
	otherRequest := proto.Clone(req).(*pb.NodeJoinRequest)
	otherRequest.CsrPem = []byte("fresh-CSR")
	if err := keys.SignJoinRequest(otherRequest); err != nil {
		t.Fatal(err)
	}
	if err := keys.VerifyJoinResponse(otherRequest, resp); err == nil {
		t.Fatal("response replay accepted for another request")
	}
	resp.CaCert = []byte("replaced-CA")
	if err := keys.VerifyJoinResponse(req, resp); err == nil {
		t.Fatal("modified enrollment response accepted")
	}
}

func TestJoinProofRequiresCompleteVersionedRing(t *testing.T) {
	a, b := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	first, err := seal.New("new", map[string][]byte{"old": a, "new": b})
	if err != nil {
		t.Fatal(err)
	}
	second, err := seal.New("new", map[string][]byte{"new": b, "old": a})
	if err != nil {
		t.Fatal(err)
	}
	req := &pb.NodeJoinRequest{Name: "node-b"}
	if err := first.SignJoinRequest(req); err != nil {
		t.Fatal(err)
	}
	if err := second.VerifyJoinRequest(req); err != nil {
		t.Fatalf("map order changed keyring identity: %v", err)
	}
	missing, err := seal.New("new", map[string][]byte{"new": b})
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.VerifyJoinRequest(req); err == nil {
		t.Fatal("peer missing historical decryption key accepted")
	}
	differentActive, err := seal.New("old", map[string][]byte{"new": b, "old": a})
	if err != nil {
		t.Fatal(err)
	}
	if err := differentActive.VerifyJoinRequest(req); err == nil {
		t.Fatal("mismatched active wrapping key accepted")
	}
}
