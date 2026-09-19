package grpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

type authorizedLogsDocker struct {
	dockerx.Docker
	opens atomic.Int32
}

func (d *authorizedLogsDocker) ContainerList(_ context.Context, opts container.ListOptions) ([]types.Container, error) {
	id := strings.TrimPrefix(opts.Filters.Get("label")[0], "jaco.replica_id=")
	return []types.Container{{ID: id}}, nil
}

func (d *authorizedLogsDocker) ContainerInspect(_ context.Context, id string) (types.ContainerJSON, error) {
	return types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{ID: id}}, nil
}

func (d *authorizedLogsDocker) ContainerLogs(_ context.Context, id string, _ container.LogsOptions) (io.ReadCloser, error) {
	d.opens.Add(1)
	var b bytes.Buffer
	_, _ = stdcopy.NewStdWriter(&b, stdcopy.Stdout).Write([]byte("private log for " + id + "\n"))
	return io.NopCloser(&b), nil
}

func receiveInternalLog(ctx context.Context, c pb.InternalClient, req *pb.LogsRequest) (*pb.LogLine, error) {
	stream, err := c.Logs(ctx, req)
	if err != nil {
		return nil, err
	}
	return stream.Recv()
}

func TestInternalRPCLogsRequireDelegation(t *testing.T) {
	s, conn := internalRPCLeader(t)
	docker := &authorizedLogsDocker{}
	s.docker = docker
	s.state.ReplicasDesired.Apply(&pb.ReplicaDesired{Id: "private-web", Deployment: "private", Service: "web", Host: "leader"}, 1)
	s.state.ReplicasDesired.Apply(&pb.ReplicaDesired{Id: "private-db", Deployment: "private", Service: "db", Host: "leader"}, 1)
	req := &pb.LogsRequest{Deployment: "private", Service: "web"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := pb.NewInternalClient(conn)
	if line, err := receiveInternalLog(ctx, client, req); status.Code(err) != codes.PermissionDenied {
		t.Errorf("ordinary member read unrelated logs: line=%v err=%v", line, err)
	}
	if docker.opens.Load() != 0 {
		t.Error("unauthorized request opened workload logs")
	}
	hash := sha256.Sum256([]byte("operator-secret"))
	tok := &pb.Token{Identity: "operator", HashedSecret: hash[:]}
	s.state.Tokens.Apply(tok, 1)
	withAuth := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer operator-secret"))
	stream, err := client.Logs(withAuth, req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := stream.Recv()
	if err != nil || line.GetReplicaId() != "private-web" {
		t.Fatalf("operator log delegation: line=%v err=%v", line, err)
	}
	if line, err := stream.Recv(); err != io.EOF {
		t.Fatalf("service filter leaked another replica: line=%v err=%v", line, err)
	}
	tok.RevokedAt = timestamppb.Now()
	s.state.Tokens.Apply(tok, 2)
	if line, err := receiveInternalLog(withAuth, client, req); status.Code(err) != codes.Unauthenticated {
		t.Errorf("revoked operator delegation: line=%v err=%v", line, err)
	}
	meta := s.state.Cluster.Get()
	s.state.Nodes.Apply(&pb.Node{Hostname: "leader"}, 1)
	leaderConn := internalRPCClient(t, s, nodeRPCCertificate(t, "leader", meta.GetCaCert(), meta.GetCaKey()))
	if line, err := receiveInternalLog(ctx, pb.NewInternalClient(leaderConn), req); err != nil || line.GetReplicaId() != "private-web" {
		t.Fatalf("current leader log fanout: line=%v err=%v", line, err)
	}
	if line, err := receiveInternalLog(withAuth, pb.NewInternalClient(leaderConn), req); status.Code(err) != codes.Unauthenticated {
		t.Errorf("invalid delegation fell back to leader authority: line=%v err=%v", line, err)
	}
}

func TestDeploymentLogsForwardsOperatorAuthority(t *testing.T) {
	target, _ := internalRPCLeader(t)
	meta := target.state.Cluster.Get()
	target.tlsDyn.swap(nodeRPCCertificate(t, "leader", meta.GetCaCert(), meta.GetCaKey()))
	target.docker = &authorizedLogsDocker{}
	rep := &pb.ReplicaDesired{Id: "private-web", Deployment: "private", Service: "web", Host: "leader"}
	target.state.ReplicasDesired.Apply(rep, 1)
	hash := sha256.Sum256([]byte("operator-secret"))
	tok := &pb.Token{Identity: "operator", HashedSecret: hash[:]}
	target.state.Tokens.Apply(tok, 1)

	source := internalRPCServer(t)
	source.cluster.hostname = "worker"
	source.dataDir = t.TempDir()
	source.state.Cluster.Set(meta, 1)
	source.state.Nodes.Apply(&pb.Node{Hostname: "leader", GrpcAddress: target.TCPAddr()}, 1)
	source.state.ReplicasDesired.Apply(rep, 1)
	source.state.Tokens.Apply(tok, 1)
	writePeerIdentity(t, source.dataDir, "worker",
		nodeRPCCertificate(t, "worker", meta.GetCaCert(), meta.GetCaKey()), meta.GetCaCert())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer operator-secret"))
	stream, err := pb.NewDeployClient(internalRPCClient(t, source)).Logs(ctx, &pb.LogsRequest{
		Deployment: "private", Service: "web",
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := stream.Recv()
	if err != nil || line.GetReplicaId() != "private-web" {
		t.Fatalf("follower operator fanout lost authority: line=%v err=%v", line, err)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("log stream termination: %v", err)
	}
	source.state.Nodes.Remove("leader", 2)
	stream, err = pb.NewDeployClient(internalRPCClient(t, source)).Logs(ctx, &pb.LogsRequest{Deployment: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if line, err := stream.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("missing destination silently produced incomplete success: line=%v err=%v", line, err)
	}
}

func TestFollowerLocalLogsRejectBeforeAnyLines(t *testing.T) {
	s := internalRPCServer(t)
	s.cluster.hostname = "worker"
	docker := &authorizedLogsDocker{}
	s.docker = docker
	s.state.ReplicasDesired.Apply(&pb.ReplicaDesired{Id: "local", Deployment: "private", Service: "web", Host: "worker"}, 1)
	s.state.ReplicasDesired.Apply(&pb.ReplicaDesired{Id: "remote", Deployment: "private", Service: "web", Host: "leader"}, 1)
	s.state.Nodes.Apply(&pb.Node{Hostname: "leader", GrpcAddress: "127.0.0.1:1"}, 1)
	conn := localRPCClient(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := pb.NewDeployClient(conn).Logs(ctx, &pb.LogsRequest{Deployment: "private"})
	if err != nil {
		t.Fatal(err)
	}
	line, err := stream.Recv()
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "operator bearer") {
		t.Fatalf("follower local fanout must reject before any line: line=%v err=%v", line, err)
	}
	if docker.opens.Load() != 0 {
		t.Fatal("unauthorized fanout opened local logs before authorization")
	}
	badAuth := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer invalid"))
	stream, err = pb.NewDeployClient(conn).Logs(badAuth, &pb.LogsRequest{Deployment: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if line, err := stream.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("local bypass accepted invalid remote delegation: line=%v err=%v", line, err)
	}
	if docker.opens.Load() != 0 {
		t.Fatal("invalid delegation opened local logs")
	}
	s.state.ReplicasDesired.Remove("remote", 2)
	stream, err = pb.NewDeployClient(conn).Logs(ctx, &pb.LogsRequest{Deployment: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if line, err := stream.Recv(); err != nil || line.GetReplicaId() != "local" {
		t.Fatalf("follower-local own logs: line=%v err=%v", line, err)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("local-only stream termination: %v", err)
	}
}

type controlledLogsDocker struct {
	authorizedLogsDocker
	proceed <-chan struct{}
}

func (d *controlledLogsDocker) ContainerLogs(ctx context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
	r, w := io.Pipe()
	go func() {
		defer w.Close()
		out := stdcopy.NewStdWriter(w, stdcopy.Stdout)
		_, _ = out.Write([]byte("before revocation\n"))
		select {
		case <-d.proceed:
			_, _ = out.Write([]byte("after revocation\n"))
		case <-ctx.Done():
		}
	}()
	return r, nil
}

func TestInternalRPCLogsRechecksAuthority(t *testing.T) {
	for _, kind := range []string{"token revoked", "member removed", "CA replaced"} {
		t.Run(kind, func(t *testing.T) {
			s, conn := internalRPCLeader(t)
			proceed := make(chan struct{})
			s.docker = &controlledLogsDocker{proceed: proceed}
			s.state.ReplicasDesired.Apply(&pb.ReplicaDesired{Id: "private-web", Deployment: "private", Service: "web", Host: "leader"}, 1)
			hash := sha256.Sum256([]byte("operator-secret"))
			tok := &pb.Token{Identity: "operator", HashedSecret: hash[:]}
			s.state.Tokens.Apply(tok, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer operator-secret"))
			stream, err := pb.NewInternalClient(conn).Logs(ctx, &pb.LogsRequest{Deployment: "private", Follow: true})
			if err != nil {
				t.Fatal(err)
			}
			if line, err := stream.Recv(); err != nil || line.GetLine() != "before revocation" {
				t.Fatalf("authorized first log: line=%v err=%v", line, err)
			}
			want := codes.Unauthenticated
			switch kind {
			case "token revoked":
				tok.RevokedAt = timestamppb.Now()
				s.state.Tokens.Apply(tok, 2)
			case "member removed":
				s.state.Nodes.Remove("worker", 2)
				want = codes.PermissionDenied
			case "CA replaced":
				newCA, _, err := ca.GenerateClusterCA()
				if err != nil {
					t.Fatal(err)
				}
				meta := s.state.Cluster.Get()
				meta.CaCert = newCA
				s.state.Cluster.Set(meta, 2)
			}
			close(proceed)
			if line, err := stream.Recv(); status.Code(err) != want {
				t.Fatalf("authority change leaked another line: line=%v err=%v, want %v", line, err, want)
			}
		})
	}
}
