package grpcsrv

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestDeployLogs_ReturnsUnimplemented — the controlplane stub is a
// placeholder; the daemon implements the real fanout in
// internal/daemon/grpc/deploy_logs.go.
func TestDeployLogs_ReturnsUnimplemented(t *testing.T) {
	d := &deployServer{state: state.New(watch.NewRegistry())}
	err := d.Logs(&pb.LogsRequest{}, nil)
	if err == nil {
		t.Fatalf("Logs returned nil err")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

// TestDeployDelete_NilRaftReturnsUnavailable — defensive guard fires
// before any state lookup.
func TestDeployDelete_NilRaftReturnsUnavailable(t *testing.T) {
	d := &deployServer{state: state.New(watch.NewRegistry())}
	_, err := d.Delete(context.TODO(), &pb.DeleteRequest{Deployment: "x"})
	if err == nil {
		t.Fatalf("Delete with nil raft: nil err")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", st.Code())
	}
}

// TestClusterBackup_NilRaftReturnsUnavailable — the controlplane
// Backup handler is leader-only; nil raft fails first.
func TestClusterBackup_NilRaftReturnsUnavailable(t *testing.T) {
	c := &clusterServer{state: state.New(watch.NewRegistry())}
	err := c.Backup(&pb.BackupRequest{}, nil)
	if err == nil {
		t.Fatalf("Backup with nil raft: nil err")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", st.Code())
	}
}
