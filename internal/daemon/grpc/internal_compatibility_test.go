package grpc

import (
	"context"
	"crypto/sha256"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func localRPCClient(t *testing.T, s *Server) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///local", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s.SocketPath())
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func internalRPCPair(t *testing.T) (*Server, *Server) {
	t.Helper()
	leader, _ := internalRPCLeader(t)
	follower := internalRPCServer(t)
	follower.cluster.hostname = "worker"
	meta := leader.state.Cluster.Get()
	follower.state.Cluster.Set(meta, 1)
	for _, s := range []*Server{leader, follower} {
		s.state.Nodes.Apply(&pb.Node{Hostname: "leader", GrpcAddress: leader.TCPAddr()}, 1)
		s.state.Nodes.Apply(&pb.Node{Hostname: "worker", GrpcAddress: follower.TCPAddr()}, 1)
		host, err := s.cluster.effectiveHostname()
		if err != nil {
			t.Fatal(err)
		}
		s.dataDir = t.TempDir()
		cert := nodeRPCCertificate(t, host, meta.GetCaCert(), meta.GetCaKey())
		s.tlsDyn.swap(cert)
		writePeerIdentity(t, s.dataDir, host, cert, meta.GetCaCert())
	}
	r, err := raftnode.New(raftnode.Config{
		DataDir: t.TempDir(), BindAddr: "127.0.0.1:0", LocalID: "worker",
		FSM: fsm.New(follower.state, watch.NewRegistry()), LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	follower.raftMu.Lock()
	follower.raft = r
	follower.raftMu.Unlock()
	if err := leader.Raft().Raft.AddNonvoter(hraft.ServerID("worker"), hraft.ServerAddress(r.LocalAddr()), 0, 5*time.Second).Error(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, id := r.Raft.LeaderWithID()
		if id == "leader" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("follower did not learn leader identity")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return leader, follower
}

func TestInternalRPCLocalUnixRetainsAuthority(t *testing.T) {
	s, _ := internalRPCLeader(t)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(s.SocketPath())
		if err != nil || info.Mode().Perm() != 0o660 {
			t.Fatalf("local socket permissions: info=%v err=%v", info, err)
		}
	}
	hash := sha256.Sum256([]byte("local-operator-secret"))
	err := submitRPCCommand(t, localRPCClient(t, s), &pb.Command{Payload: &pb.Command_TokenIssue{TokenIssue: &pb.TokenIssue{
		Identity: "local-operator", HashedSecret: hash[:], AllowsPrivileged: true,
	}}})
	if err != nil {
		t.Fatalf("filesystem-authorized local control: %v", err)
	}
	if tok, ok := s.state.Tokens.Get("local-operator"); !ok || !tok.GetAllowsPrivileged() {
		t.Fatal("local operator command did not persist")
	}
}

func TestLeaderLocalLogsFanout(t *testing.T) {
	leader, follower := internalRPCPair(t)
	local := &pb.ReplicaDesired{Id: "local-web", Deployment: "private", Service: "web", Host: "leader"}
	remote := &pb.ReplicaDesired{Id: "remote-web", Deployment: "private", Service: "web", Host: "worker"}
	for _, s := range []*Server{leader, follower} {
		s.docker = &authorizedLogsDocker{}
		s.state.ReplicasDesired.Apply(local, 1)
		s.state.ReplicasDesired.Apply(remote, 1)
		s.state.ReplicasDesired.Apply(&pb.ReplicaDesired{Id: "other-service", Deployment: "private", Service: "db", Host: "worker"}, 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := pb.NewDeployClient(localRPCClient(t, leader)).Logs(ctx, &pb.LogsRequest{Deployment: "private", Service: "web"})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for {
		line, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[line.GetReplicaId()] = true
	}
	if len(seen) != 2 || !seen["local-web"] || !seen["remote-web"] {
		t.Fatalf("leader-local fanout or service isolation failed: %v", seen)
	}
}

func TestOperatorRetriesFollowerWithOriginalBearer(t *testing.T) {
	leader, follower := internalRPCPair(t)
	hash := sha256.Sum256([]byte("operator-secret"))
	for _, s := range []*Server{leader, follower} {
		s.state.Tokens.Apply(&pb.Token{Identity: "operator", HashedSecret: hash[:]}, 1)
		s.tokens.set(grpcsrv.NewTokensServer(s.state, s.Raft()))
	}
	client, err := cliclient.NewClient(&cliclient.Context{
		ServerAddrs: []string{follower.TCPAddr(), leader.TCPAddr()},
		Token:       "operator-secret",
		CACertPath:  filepath.Join(leader.dataDir, "node", "ca.crt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attempts := 0
	err = client.Invoke(ctx, func(conn *grpc.ClientConn) error {
		attempts++
		_, err := pb.NewTokensClient(conn).Issue(client.AuthContext(ctx), &pb.TokenIssueRequest{Identity: "retried-operator"})
		if attempts == 1 && (status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "no_leader") {
			t.Errorf("follower response: %v, want no_leader", err)
		}
		return err
	})
	if err != nil || attempts != 2 {
		t.Fatalf("operator endpoint retry: attempts=%d err=%v", attempts, err)
	}
	if _, ok := leader.state.Tokens.Get("retried-operator"); !ok {
		t.Fatal("retried administrative command did not persist")
	}
}
