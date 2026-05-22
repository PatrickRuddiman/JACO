package grpcsrv

import (
	"bytes"

	"google.golang.org/grpc/codes"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/backup"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Backup streams the cluster's backup tarball back to the caller in chunks.
// Requires operator authentication (the admission interceptor handles that)
// and a live raft leader (Export drives raft.Snapshot which must run on the
// leader; on followers Snapshot still works but only of the current state, so
// for v1 we gate on leader to keep semantics obvious).
func (c *clusterServer) Backup(_ *pb.BackupRequest, stream pb.Cluster_BackupServer) error {
	if c.raft == nil {
		return errorStatus(codes.Unavailable, "raft_unavailable", "raft not wired")
	}
	if !c.raft.IsLeader() {
		return errorStatus(codes.Unavailable, "no_leader", "backup requires leader")
	}

	meta := c.state.Cluster.Get()
	if meta == nil {
		return errorStatus(codes.FailedPrecondition, "no_cluster", "cluster meta not yet present")
	}

	var buf bytes.Buffer
	if err := backup.Export(backup.ExportOptions{
		Raft:        c.raft,
		ClusterID:   meta.GetClusterId(),
		JacoVersion: backupJacoVersion,
		Identity:    admission.IdentityFromContext(stream.Context()),
		Writer:      &buf,
	}); err != nil {
		return errorStatus(codes.Internal, "backup_failed", err.Error())
	}

	const chunkSize = 64 * 1024
	data := buf.Bytes()
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		if err := stream.Send(&pb.BackupChunk{Data: data[i:end]}); err != nil {
			return err
		}
	}
	return nil
}

// Restore is intentionally Unimplemented at the gRPC layer in v1. The operator
// runs `jaco restore` locally on the receiving node (it operates on the data
// directory and exits before `jaco serve` starts), so there's no streaming
// path through the cluster while it's down. The Cluster.Restore RPC is
// reserved for a future in-flight restore flow.

// backupJacoVersion is the version string embedded in backup metadata. Task 35
// will wire this via `-ldflags "-X ..."` against the real build; for now a
// dev placeholder.
const backupJacoVersion = "0.0.1-dev"
