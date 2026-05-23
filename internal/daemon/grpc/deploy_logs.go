package grpc

import (
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/runtime/lifecycle"
	"github.com/PatrickRuddiman/jaco/internal/runtime/logs"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// streamLocalLogs is the daemon-side Deploy.Logs implementation. v0 ships
// local-only fanout: streams logs for any replica whose Host equals this
// node's hostname and skips remote replicas. Cross-host fanout
// (Internal.Logs) is a follow-up — the operator can SSH to each node and
// run `jaco logs` locally in the meantime if a multi-node tail is needed.
func (s *Server) streamLocalLogs(req *pb.LogsRequest, stream pb.Deploy_LogsServer) error {
	if s.docker == nil {
		return status.Error(codes.Unavailable, "logs_unavailable: docker handle not wired")
	}
	st := s.State()
	if st == nil {
		return status.Error(codes.Unavailable, "state_unavailable")
	}
	hostname := s.cluster.hostname
	if hostname == "" {
		return status.Error(codes.Internal, "hostname_not_resolved")
	}

	wanted := req.GetDeployment()
	if wanted == "" {
		return status.Error(codes.InvalidArgument, "deployment is required")
	}
	wantedService := req.GetService()

	// Gather local replicas matching the filter.
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
		// Nothing to stream from this host — return cleanly. The
		// operator can read state.Nodes to see which hosts have the
		// replicas they're after.
		return nil
	}

	opts := logs.Options{Follow: req.GetFollow()}
	if s := req.GetSinceSeconds(); s > 0 {
		opts.Since = time.Duration(s) * time.Second
	}

	// Fan in: each replica's log channel funnels into the common gRPC
	// stream. Errors from any single replica don't abort the others.
	var wg sync.WaitGroup
	errCh := make(chan error, len(localReps))

	for _, rep := range localReps {
		rep := rep
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Resolve container id via lifecycle.Inspect.
			containerID, _, err := lifecycle.Inspect(stream.Context(), s.docker, rep.GetId())
			if err != nil || containerID == "" {
				return
			}
			ch, err := logs.Stream(stream.Context(), s.docker, rep.GetId(), containerID, hostname, opts)
			if err != nil {
				errCh <- err
				return
			}
			for ll := range ch {
				if err := stream.Send(ll); err != nil {
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
