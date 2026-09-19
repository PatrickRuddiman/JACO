package grpc

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/ipam"
	"github.com/PatrickRuddiman/jaco/internal/ingress/challenge"
	"github.com/PatrickRuddiman/jaco/internal/ingress/storage"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func internalRPCServer(t *testing.T) *Server {
	t.Helper()
	dir, err := os.MkdirTemp("", "jaco-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dir) })
	s, err := New(Options{
		UnixSocketPath: filepath.Join(dir, "control.sock"),
		ListenAddr:     net.JoinHostPort("127.0.0.1", "0"),
		Hostname:       "leader",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.state = state.New(watch.NewRegistry())
	s.Gate().MarkInitialized()
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Stop(ctx)
		if err := <-serveDone; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return s
}

func internalRPCClient(t *testing.T, s *Server, certs ...tls.Certificate) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(s.TCPAddr(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		// This fixture uses the daemon's short-lived bootstrap server cert.
		InsecureSkipVerify: true,
		Certificates:       certs,
	})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestInternalRPCAnonymousTLSRejected(t *testing.T) {
	s := internalRPCServer(t)
	conn := internalRPCClient(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pb.NewInternalClient(conn).Submit(ctx, &pb.SubmitRequest{CommandBytes: []byte{1}})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous Internal.Submit = %v, want Unauthenticated", err)
	}
}

func nodeRPCCertificate(t *testing.T, hostname string, caCert, caKey []byte) tls.Certificate {
	t.Helper()
	key, csr, err := ca.GenerateNodeKeypair(hostname, net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignNodeCSR(csr, caCert, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, key)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestInternalRPCMemberTLSIdentity(t *testing.T) {
	s := internalRPCServer(t)
	caCert, caKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	s.state.Cluster.Set(&pb.ClusterMeta{ClusterId: "test", CaCert: caCert}, 1)
	s.state.Nodes.Apply(&pb.Node{Hostname: "worker"}, 1)
	conn := internalRPCClient(t, s, nodeRPCCertificate(t, "worker", caCert, caKey))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = pb.NewInternalClient(conn).SignNodeCert(ctx, &pb.SignNodeCertRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("member client certificate = %v, want admitted to unimplemented handler", err)
	}
}

func internalRPCLeader(t *testing.T) (*Server, *grpc.ClientConn) {
	t.Helper()
	s := internalRPCServer(t)
	caCert, caKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	s.state.Cluster.Set(&pb.ClusterMeta{ClusterId: "test", CaCert: caCert, CaKey: caKey}, 1)
	s.state.Nodes.Apply(&pb.Node{Hostname: "worker", Status: pb.NodeStatus_NODE_STATUS_READY}, 1)
	r, err := raftnode.New(raftnode.Config{
		DataDir:   t.TempDir(),
		BindAddr:  "127.0.0.1:0",
		LocalID:   "leader",
		Bootstrap: true,
		FSM:       fsm.New(s.state, watch.NewRegistry()),
		LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.raftMu.Lock()
	s.raft = r
	s.raftMu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for !r.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("raft did not elect the fixture leader")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.Raft.Barrier(5 * time.Second).Error(); err != nil {
		t.Fatalf("raft fixture did not apply its initial leadership log: %v", err)
	}
	return s, internalRPCClient(t, s, nodeRPCCertificate(t, "worker", caCert, caKey))
}

func submitRPCCommand(t *testing.T, conn *grpc.ClientConn, cmd *pb.Command) error {
	t.Helper()
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = pb.NewInternalClient(conn).Submit(ctx, &pb.SubmitRequest{CommandBytes: data})
	return err
}

func TestInternalRPCRejectsAdministrativeCommands(t *testing.T) {
	s, conn := internalRPCLeader(t)
	hash := sha256.Sum256([]byte("attacker-chosen-test-token"))
	token := &pb.Command{Payload: &pb.Command_TokenIssue{TokenIssue: &pb.TokenIssue{
		Identity: "injected", HashedSecret: hash[:], AllowsPrivileged: true,
	}}}
	cases := map[string]*pb.Command{
		"token": token,
		"deployment": {Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
			Deployment: "injected", ComposeYaml: []byte("arbitrary"),
		}}},
		"batch": {Payload: &pb.Command_Batch{Batch: &pb.Batch{Children: []*pb.Command{token}}}},
		"nested batch": {Payload: &pb.Command_Batch{Batch: &pb.Batch{Children: []*pb.Command{
			{Payload: &pb.Command_Batch{Batch: &pb.Batch{Children: []*pb.Command{token}}}},
		}}}},
	}
	for name, cmd := range cases {
		t.Run(name, func(t *testing.T) {
			if err := submitRPCCommand(t, conn, cmd); status.Code(err) != codes.PermissionDenied {
				t.Errorf("member Submit = %v, want PermissionDenied", err)
			}
		})
	}
	if _, exists := s.state.Tokens.Get("injected"); exists {
		t.Error("peer created an administrative token")
	}
	if _, exists := s.state.Deployments.Get("injected"); exists {
		t.Error("peer bypassed deployment admission")
	}
}

func TestInternalRPCBindsWorkerMutations(t *testing.T) {
	s, conn := internalRPCLeader(t)
	s.state.Nodes.Apply(&pb.Node{Hostname: "other", Status: pb.NodeStatus_NODE_STATUS_READY}, 1)
	s.state.ReplicasDesired.Apply(&pb.ReplicaDesired{Id: "owned", Host: "worker", Deployment: "app", Service: "web"}, 1)
	s.state.ReplicasDesired.Apply(&pb.ReplicaDesired{Id: "foreign", Host: "other", Deployment: "app", Service: "web"}, 1)
	forbidden := map[string]*pb.Command{
		"other node status": {Payload: &pb.Command_NodeStatusUpdate{NodeStatusUpdate: &pb.NodeStatusUpdate{Hostname: "other"}}},
		"other node address": {Payload: &pb.Command_NodeUpdateSelf{NodeUpdateSelf: &pb.NodeUpdateSelf{
			Hostname: "other", GrpcAddress: "127.0.0.1:7000",
		}}},
		"address outside certificate": {Payload: &pb.Command_NodeUpdateSelf{NodeUpdateSelf: &pb.NodeUpdateSelf{
			Hostname: "worker", GrpcAddress: "127.0.0.2:7000",
		}}},
		"other assignment": {Payload: &pb.Command_ReplicaObservedUpdate{ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{
			Replica: &pb.ReplicaObserved{Id: "foreign", Host: "worker", State: pb.ReplicaState_REPLICA_STATE_RUNNING},
		}}},
		"forged observation host": {Payload: &pb.Command_ReplicaObservedUpdate{ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{
			Replica: &pb.ReplicaObserved{Id: "owned", Host: "other", State: pb.ReplicaState_REPLICA_STATE_RUNNING},
		}}},
		"forged administrative audit": {Payload: &pb.Command_AuditAppend{AuditAppend: &pb.AuditAppend{
			Event: &pb.AuditEvent{Type: pb.AuditEventType_AUDIT_EVENT_TYPE_TOKEN_ISSUE},
		}}},
	}
	for name, cmd := range forbidden {
		t.Run(name, func(t *testing.T) {
			if err := submitRPCCommand(t, conn, cmd); status.Code(err) != codes.PermissionDenied {
				t.Errorf("Submit = %v, want PermissionDenied", err)
			}
		})
	}
	future := timestamppb.New(time.Now().Add(24 * time.Hour))
	allowed := []*pb.Command{
		{Identity: "bootstrap", Ts: future, Payload: &pb.Command_NodeStatusUpdate{NodeStatusUpdate: &pb.NodeStatusUpdate{
			Hostname: "worker", IncludePressure: true, CpuPressure: 0.4, MemoryPressure: 0.3,
		}}},
		{Payload: &pb.Command_NodeUpdateSelf{NodeUpdateSelf: &pb.NodeUpdateSelf{
			Hostname: "worker", GrpcAddress: "127.0.0.1:7000",
		}}},
		{Payload: &pb.Command_ReplicaObservedUpdate{ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{
			Replica: &pb.ReplicaObserved{
				Id: "owned", State: pb.ReplicaState_REPLICA_STATE_RUNNING, LastHealthAt: future,
				Details: map[string]string{"host": "other", "deployment": "forged"},
			},
		}}},
		{Identity: "bootstrap", Ts: future, Payload: &pb.Command_AuditAppend{AuditAppend: &pb.AuditAppend{
			Event: &pb.AuditEvent{
				Type: pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_UNAVAILABLE, Identity: "bootstrap", Ts: future,
				Payload: map[string]string{"host": "other", "hostname": "other", "identity": "bootstrap"},
			},
		}}},
	}
	start := time.Now()
	for _, cmd := range allowed {
		if err := submitRPCCommand(t, conn, cmd); err != nil {
			t.Fatalf("legitimate worker command: %v", err)
		}
	}
	node, _ := s.state.Nodes.Get("worker")
	if node.GetLastPressureAt().AsTime().Before(start) || node.GetLastPressureAt().AsTime().After(time.Now()) {
		t.Error("caller controlled the pressure timestamp")
	}
	obs, _ := s.state.ReplicasObserved.Get("owned")
	if obs.GetHost() != "worker" || obs.GetDetails()["host"] != "worker" || obs.GetDetails()["deployment"] != "app" {
		t.Errorf("observation not bound to assignment: %v", obs)
	}
	if obs.GetLastHealthAt().AsTime().Before(start) || obs.GetLastHealthAt().AsTime().After(time.Now()) {
		t.Error("caller controlled the health timestamp")
	}
	events := s.state.AuditEvents.List()
	event := events[len(events)-1]
	if event.GetIdentity() != "node:worker" || event.GetPayload()["host"] != "worker" ||
		event.GetPayload()["hostname"] != "worker" || event.GetPayload()["identity"] != "node:worker" {
		t.Errorf("audit not bound to caller: %v", event)
	}
	if event.GetTs().AsTime().Before(start) || event.GetTs().AsTime().After(time.Now()) {
		t.Error("caller controlled the audit timestamp")
	}
}

func TestInternalRPCSubnetScope(t *testing.T) {
	s, conn := internalRPCLeader(t)
	allocator, err := ipam.New(s.state, func(data []byte) error {
		_, err := s.Raft().Apply(data, 0)
		return err
	}, ipam.DefaultPoolCIDR)
	if err != nil {
		t.Fatal(err)
	}
	s.raftMu.Lock()
	s.ipamAllocator = allocator
	s.raftMu.Unlock()
	s.state.Deployments.Apply(&pb.Deployment{
		Name:        "app",
		ComposeYaml: []byte("services:\n  web:\n    image: nginx\n    networks: [backend]\n  api:\n    image: nginx\nnetworks:\n  backend: {}\n"),
		Services:    []*pb.ServiceSpec{{Name: "web", Networks: []string{"backend"}}, {Name: "api"}},
	}, 1)
	s.state.ReplicasDesired.Apply(&pb.ReplicaDesired{Id: "owned", Host: "worker", Deployment: "app", Service: "web"}, 1)
	s.state.ReplicasDesired.Apply(&pb.ReplicaDesired{Id: "default", Host: "worker", Deployment: "app", Service: "api"}, 1)
	c := pb.NewInternalClient(conn)
	for _, req := range []*pb.EnsureSubnetRequest{
		{Deployment: "app", Network: "backend", Host: "other"},
		{Deployment: "unassigned", Network: "backend", Host: "worker"},
		{Deployment: "app", Network: "invented", Host: "worker"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := c.EnsureSubnet(ctx, req)
		cancel()
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("EnsureSubnet(%v) = %v, want PermissionDenied", req, err)
		}
	}
	if s.state.Subnets.Len() != 0 {
		t.Error("unauthorized subnet allocation changed state")
	}
	for _, network := range []string{"backend", "_default"} {
		req := &pb.EnsureSubnetRequest{Deployment: "app", Network: network, Host: "worker"}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		first, err := c.EnsureSubnet(ctx, req)
		if err != nil {
			cancel()
			t.Fatalf("assigned network %q: %v", network, err)
		}
		second, err := c.EnsureSubnet(ctx, req)
		cancel()
		if err != nil || first.GetCidr() == "" || first.GetCidr() != second.GetCidr() {
			t.Fatalf("assigned network %q not idempotent: first=%v second=%v err=%v", network, first, second, err)
		}
	}
}

func TestInternalRPCACMEScope(t *testing.T) {
	s, conn := internalRPCLeader(t)
	s.state.Routes.Apply(&pb.Route{Domain: "app.example", TlsAuto: true}, 1)
	future := timestamppb.New(time.Now().Add(24 * time.Hour))
	s.state.Certs.Apply(&pb.Cert{Domain: "foreign", Lessee: "other", LockUntil: future}, 1)
	for name, cmd := range map[string]*pb.Command{
		"foreign lessee": {Payload: &pb.Command_CertLock{CertLock: &pb.CertLock{Name: "forged", Lessee: "other", Until: future}}},
		"foreign unlock": {Payload: &pb.Command_CertUnlock{CertUnlock: &pb.CertUnlock{Name: "foreign"}}},
		"unconfigured challenge": {Payload: &pb.Command_ChallengeTokenStore{ChallengeTokenStore: &pb.ChallengeTokenStore{
			Token: &pb.ChallengeToken{Token: "unrelated", Domain: "unrelated.example", KeyAuth: "proof", ExpiresAt: future},
		}}},
		"unconfigured audit": {Payload: &pb.Command_AuditAppend{AuditAppend: &pb.AuditAppend{
			Event: &pb.AuditEvent{Type: pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_FAILED, Payload: map[string]string{"domain": "unrelated.example"}},
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := submitRPCCommand(t, conn, cmd); status.Code(err) != codes.PermissionDenied {
				t.Errorf("Submit = %v, want PermissionDenied", err)
			}
		})
	}
	if err := submitRPCCommand(t, conn, &pb.Command{Payload: &pb.Command_CertLock{CertLock: &pb.CertLock{
		Name: "owned", Lessee: "worker", Until: future,
	}}}); err != nil {
		t.Fatal(err)
	}
	lock, _ := s.state.Certs.Get("owned")
	if lock.GetLessee() != "worker" || lock.GetLockUntil().AsTime().After(time.Now().Add(storage.LockTTL+time.Second)) {
		t.Errorf("unbounded or unbound lease: %v", lock)
	}
	if err := submitRPCCommand(t, conn, &pb.Command{Payload: &pb.Command_ChallengeTokenStore{ChallengeTokenStore: &pb.ChallengeTokenStore{
		Token: &pb.ChallengeToken{Token: "valid", Domain: "app.example", KeyAuth: "valid.proof", ExpiresAt: future},
	}}}); err != nil {
		t.Fatal(err)
	}
	token, _ := s.state.ChallengeTokens.Get("valid")
	if token.GetExpiresAt().AsTime().After(time.Now().Add(challenge.TokenTTL + time.Second)) {
		t.Error("unbounded challenge lifetime")
	}
	store := storage.New(s.state, func(data []byte) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := pb.NewInternalClient(conn).Submit(ctx, &pb.SubmitRequest{CommandBytes: data})
		return err
	}, "worker", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := store.Lock(ctx, "owned"); err != nil {
		t.Fatalf("certmagic lock forwarding: %v", err)
	}
	for _, key := range []string{
		"certificates/acme.test-directory/app.example/app.example.crt",
		"acme/acme.test-directory/users/operator/account.json",
		"ocsp/app.example",
		"last_clean.json",
	} {
		if err := store.Store(ctx, key, []byte("blob")); err != nil {
			t.Fatalf("certmagic Store(%q): %v", key, err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("certmagic Delete(%q): %v", key, err)
		}
	}
	if err := store.Unlock(ctx, "owned"); err != nil {
		t.Fatalf("certmagic unlock forwarding: %v", err)
	}
	lock, _ = s.state.Certs.Get("owned")
	if lock.GetLessee() != "" {
		t.Error("owner could not release its lease")
	}
}
