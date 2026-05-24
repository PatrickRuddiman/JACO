package grpc

import (
	"context"
	"crypto/tls"
	"io"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	"github.com/PatrickRuddiman/jaco/internal/runtime/lifecycle"
	"github.com/PatrickRuddiman/jaco/internal/runtime/logs"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// logsSender is the narrow stream-like interface streamLocalLogs needs:
// just Send + Context. grpc.ServerStreamingServer[LogLine] satisfies this
// directly, so Internal.Logs handlers can pass their stream through; the
// operator-facing Deploy.Logs wraps it with a mutex so multiple
// concurrent fanout goroutines can share one underlying stream safely.
type logsSender interface {
	Send(*pb.LogLine) error
	Context() context.Context
}

// streamDeploymentLogs is the operator-facing fanout: streams local
// replica logs directly + dials Internal.Logs on every peer hosting
// remote replicas. Used by deployProxy.Logs (Deploy.Logs RPC).
func (s *Server) streamDeploymentLogs(req *pb.LogsRequest, stream pb.Deploy_LogsServer) error {
	st := s.State()
	if st == nil {
		return status.Error(codes.Unavailable, "state_unavailable")
	}
	hostname, err := s.cluster.effectiveHostname()
	if err != nil {
		return status.Errorf(codes.Internal, "hostname: %v", err)
	}

	hosts := map[string]bool{}
	for _, r := range st.ReplicasDesired.List() {
		if r.GetDeployment() != req.GetDeployment() {
			continue
		}
		if svc := req.GetService(); svc != "" && r.GetService() != svc {
			continue
		}
		hosts[r.GetHost()] = true
	}

	var sendMu sync.Mutex
	safe := func(ll *pb.LogLine) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(ll)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(hosts))

	for h := range hosts {
		h := h
		wg.Add(1)
		if h == hostname {
			go func() {
				defer wg.Done()
				ms := mutexSender{ctx: stream.Context(), send: safe}
				if err := s.streamLocalLogs(req, ms); err != nil {
					errCh <- err
				}
			}()
			continue
		}
		var addr string
		for _, candidate := range st.Nodes.List() {
			if candidate.GetHostname() == h {
				addr = candidate.GetGrpcAddress()
				break
			}
		}
		if addr == "" {
			wg.Done()
			continue
		}
		go func(addr string) {
			defer wg.Done()
			conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
			if err != nil {
				errCh <- err
				return
			}
			defer conn.Close()
			peerStream, err := pb.NewInternalClient(conn).Logs(stream.Context(), req)
			if err != nil {
				errCh <- err
				return
			}
			for {
				ll, err := peerStream.Recv()
				if err == io.EOF {
					return
				}
				if err != nil {
					errCh <- err
					return
				}
				if err := safe(ll); err != nil {
					errCh <- err
					return
				}
			}
		}(addr)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return status.Errorf(codes.Internal, "logs_stream: %v", err)
		}
	}
	return nil
}

// mutexSender adapts a Send callback into the logsSender interface so
// streamLocalLogs can multiplex through the shared mutex.
type mutexSender struct {
	ctx  context.Context
	send func(*pb.LogLine) error
}

func (m mutexSender) Context() context.Context  { return m.ctx }
func (m mutexSender) Send(ll *pb.LogLine) error { return m.send(ll) }

// streamLocalLogs streams logs for replicas whose Host equals this
// node's hostname. Called directly by Internal.Logs and indirectly via
// streamDeploymentLogs for the leader's own local replicas.
func (s *Server) streamLocalLogs(req *pb.LogsRequest, sender logsSender) error {
	if s.docker == nil {
		return status.Error(codes.Unavailable, "logs_unavailable: docker handle not wired")
	}
	st := s.State()
	if st == nil {
		return status.Error(codes.Unavailable, "state_unavailable")
	}
	hostname, err := s.cluster.effectiveHostname()
	if err != nil {
		return status.Errorf(codes.Internal, "hostname: %v", err)
	}

	wanted := req.GetDeployment()
	if err := grpcsrv.ValidateDeploymentName(wanted); err != nil {
		return err
	}
	wantedService := req.GetService()

	var localReps []*pb.ReplicaDesired
	for _, r := range st.ReplicasDesired.List() {
		if r.GetHost() != hostname {
			continue
		}
		if r.GetDeployment() != wanted {
			continue
		}
		if wantedService != "" && r.GetService() != wantedService {
			continue
		}
		localReps = append(localReps, r)
	}
	if len(localReps) == 0 {
		return nil
	}

	opts := logs.Options{Follow: req.GetFollow()}
	if s := req.GetSinceSeconds(); s > 0 {
		opts.Since = time.Duration(s) * time.Second
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(localReps))

	for _, rep := range localReps {
		rep := rep
		wg.Add(1)
		go func() {
			defer wg.Done()
			containerID, _, err := lifecycle.Inspect(sender.Context(), s.docker, rep.GetId())
			if err != nil || containerID == "" {
				return
			}
			ch, err := logs.Stream(sender.Context(), s.docker, rep.GetId(), containerID, hostname, opts)
			if err != nil {
				errCh <- err
				return
			}
			for ll := range ch {
				if err := sender.Send(ll); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			return status.Errorf(codes.Internal, "logs_stream: %v", err)
		}
	}
	return nil
}
