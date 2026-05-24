package grpcsrv

import (
	"context"
	"errors"
	"fmt"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// deployServer implements the jaco.v1.Deploy gRPC service for Apply, Rollback,
// and Delete. Status + Logs are added in later tasks (24, 19).
type deployServer struct {
	pb.UnimplementedDeployServer
	state *state.State
	raft  *raftnode.Node
}

// ValidateDeploymentName returns a typed InvalidArgument error when name is
// empty. Shared by every Deploy.* RPC and by the daemon-side Deploy.Logs
// handler so the wire-level error shape stays uniform.
func ValidateDeploymentName(name string) error {
	if name == "" {
		return errorStatus(codes.InvalidArgument, "validation_failed", "deployment is required")
	}
	return nil
}

// enumerateNetworks returns the union of network names declared across
// services. Empty services (no network field) get a "_default" entry so
// the daemon's discovery layer still allocates a subnet for them.
func enumerateNetworks(services []JacoServiceDecl) []string {
	seen := map[string]bool{}
	for _, s := range services {
		if len(s.Networks) == 0 {
			seen["_default"] = true
			continue
		}
		for _, n := range s.Networks {
			seen[n] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	return out
}

// Apply parses + validates the jaco.yaml + compose.yaml, then either returns
// a Diff (dry_run) or raft-Applies a DeploymentApply command whose FSM hook
// writes the Deployment + Routes entities (scheduler-driven ReplicaDesired
// derivation lands in task 21). Requires raft leader.
func (d *deployServer) Apply(ctx context.Context, req *pb.ApplyRequest) (*pb.ApplyResponse, error) {
	if d.raft == nil {
		return nil, errorStatus(codes.Unavailable, "raft_unavailable", "raft not wired")
	}
	if !d.raft.IsLeader() {
		return nil, errorStatus(codes.Unavailable, "no_leader", "apply requires leader")
	}

	jacoSpec, composeProject, err := parseAndValidate(req.GetJacoYaml(), req.GetComposeYaml())
	if err != nil {
		var ve *validationFault
		if errors.As(err, &ve) {
			return nil, errorStatus(codes.InvalidArgument, ve.Code, ve.Message)
		}
		return nil, errorStatus(codes.InvalidArgument, "validation_failed", err.Error())
	}

	// Confirm every service name matches a key in the compose file.
	composeNames := map[string]bool{}
	for _, s := range composeProject.Services {
		composeNames[s.Name] = true
	}
	for _, s := range jacoSpec.Services {
		if !composeNames[s.Name] {
			return nil, errorStatus(codes.InvalidArgument, "validation_failed",
				fmt.Sprintf("service %q not found in the compose file; name must equal a compose service key",
					s.Name))
		}
	}

	currentRev := uint64(0)
	if existing, ok := d.state.Deployments.Get(jacoSpec.Deployment); ok {
		currentRev = existing.GetAppliedRevision()
	}
	targetRev := currentRev + 1

	diff := computeDiff(d.state, jacoSpec)

	if req.GetDryRun() {
		return &pb.ApplyResponse{
			AppliedRevision: currentRev, // unchanged
			Diff:            diff,
		}, nil
	}

	// Subnet allocation is no longer done at apply time: with per-host /24s
	// (issue #28) the leader can't pick a host until the scheduler places a
	// replica, so the runtime reconciler allocates lazily via
	// Internal.EnsureSubnet on first placement.

	cmd := &pb.Command{
		Identity: admission.IdentityFromContext(ctx),
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
			Deployment:  jacoSpec.Deployment,
			Revision:    targetRev,
			JacoYaml:    req.GetJacoYaml(),
			ComposeYaml: req.GetComposeYaml(),
			Services:    toServiceSpecs(jacoSpec.Services),
			Routes:      toRoutes(jacoSpec.Deployment, jacoSpec.Routes),
		}},
	}
	if err := applyRaftCommand(d.raft, cmd); err != nil {
		return nil, errorStatus(codes.Internal, "raft_apply_failed", err.Error())
	}

	return &pb.ApplyResponse{
		AppliedRevision: targetRev,
		Diff:            diff,
	}, nil
}

// Rollback swaps Deployment.applied_revision with previous_revision and
// audits ROLLBACK. v1 only flips the revision markers; full state restore
// (re-derive services/routes from the previous revision's YAML) requires
// revision history which lands in a follow-up.
func (d *deployServer) Rollback(ctx context.Context, req *pb.RollbackRequest) (*pb.RollbackResponse, error) {
	if d.raft == nil {
		return nil, errorStatus(codes.Unavailable, "raft_unavailable", "raft not wired")
	}
	if !d.raft.IsLeader() {
		return nil, errorStatus(codes.Unavailable, "no_leader", "rollback requires leader")
	}
	name := req.GetDeployment()
	if err := ValidateDeploymentName(name); err != nil {
		return nil, err
	}
	dep, ok := d.state.Deployments.Get(name)
	if !ok {
		return nil, errorStatus(codes.NotFound, "deployment_not_found",
			fmt.Sprintf("deployment %q not found", name))
	}
	prev := dep.GetPreviousRevision()
	if prev == 0 {
		return nil, errorStatus(codes.FailedPrecondition, "no_previous_revision",
			fmt.Sprintf("deployment %q has no previous revision to roll back to", name))
	}

	cmd := &pb.Command{
		Identity: admission.IdentityFromContext(ctx),
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_DeploymentRollback{DeploymentRollback: &pb.DeploymentRollback{
			Deployment: name,
			Revision:   prev,
		}},
	}
	if err := applyRaftCommand(d.raft, cmd); err != nil {
		return nil, errorStatus(codes.Internal, "raft_apply_failed", err.Error())
	}
	return &pb.RollbackResponse{Revision: prev}, nil
}

// Logs is the operator-facing fanout RPC. v1 returns Unimplemented — the
// per-replica peer fanout (Internal.Logs over the cluster CA) needs the
// daemon entry to wire a *dockerx.Client and the local hostname into the
// server Options. runtime/logs.Stream + the jaco logs CLI are already in
// place; this stub lands the surface so the CLI receives a clean
// codes.Unimplemented + message instead of a panic.
func (d *deployServer) Logs(_ *pb.LogsRequest, _ pb.Deploy_LogsServer) error {
	return errorStatus(codes.Unimplemented, "logs_unimplemented",
		"Deploy.Logs requires daemon-side docker wiring; follow-up task")
}

// Delete raft-Applies a DeploymentDelete command. The FSM cascades — removes
// all Routes and ReplicaDesired entries owned by the deployment in addition
// to the Deployment itself.
func (d *deployServer) Delete(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	if d.raft == nil {
		return nil, errorStatus(codes.Unavailable, "raft_unavailable", "raft not wired")
	}
	if !d.raft.IsLeader() {
		return nil, errorStatus(codes.Unavailable, "no_leader", "delete requires leader")
	}
	name := req.GetDeployment()
	if err := ValidateDeploymentName(name); err != nil {
		return nil, err
	}
	cmd := &pb.Command{
		Identity: admission.IdentityFromContext(ctx),
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_DeploymentDelete{DeploymentDelete: &pb.DeploymentDelete{
			Deployment: name,
		}},
	}
	if err := applyRaftCommand(d.raft, cmd); err != nil {
		return nil, errorStatus(codes.Internal, "raft_apply_failed", err.Error())
	}
	return &pb.DeleteResponse{}, nil
}

// parseAndValidate runs both YAML files through their parsers and intrinsic
// validators. Returns the JacoYAML + compose project on success.
func parseAndValidate(jacoYAML, composeYAML []byte) (*JacoYAML, *composetypes.Project, error) {
	if len(jacoYAML) == 0 {
		return nil, nil, &validationFault{Code: "validation_failed", Message: "jaco_yaml is required"}
	}
	if len(composeYAML) == 0 {
		return nil, nil, &validationFault{Code: "validation_failed", Message: "compose_yaml is required"}
	}
	jaco, err := ParseJacoYAML(jacoYAML)
	if err != nil {
		return nil, nil, &validationFault{Code: "validation_failed", Message: err.Error()}
	}
	if code, msg, ok := validateJacoYAML(jaco); !ok {
		return nil, nil, &validationFault{Code: code, Message: msg}
	}
	if err := compose.Validate(composeYAML); err != nil {
		var ve *compose.ValidationError
		if errors.As(err, &ve) {
			return nil, nil, &validationFault{Code: ve.Code, Message: ve.Message}
		}
		return nil, nil, &validationFault{Code: "validation_failed", Message: err.Error()}
	}
	project, err := compose.LoadBytes(composeYAML, "deploy-compose.yml")
	if err != nil {
		return nil, nil, &validationFault{Code: "validation_failed", Message: err.Error()}
	}
	return jaco, project, nil
}

// computeDiff is the per-Apply replica/route/subnet projection. v1 returns an
// empty Diff — the scheduler (task 21) is the one that materializes
// ReplicaDesired entries from the Deployment, and IPAM (task 25) allocates
// the subnets. Once those land, this function can populate adds/updates/
// removes against the existing replica + subnet state.
func computeDiff(_ *state.State, _ *JacoYAML) *pb.Diff {
	return &pb.Diff{}
}

// validationFault is the package-local error type carrying the typed code +
// message back to the gRPC layer.
type validationFault struct {
	Code    string
	Message string
}

func (e *validationFault) Error() string { return e.Message }
