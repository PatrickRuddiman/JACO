package grpcsrv

import (
	"fmt"
	"log/slog"
	"sort"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// privilegedService captures one compose service that has tripped the
// `privileged:` / `security_opt:` gate. The fields slice records which of
// the two gated keys were actually set so the rejection message and the
// audit event both spell out the trigger precisely.
type privilegedService struct {
	Name   string
	Fields []string // sorted: ["privileged"], ["security_opt"], or both
}

// collectPrivilegedServices returns the services in the project that set
// `privileged: true` or a non-empty `security_opt:` list, in deterministic
// (alphabetic) order so the audit event ordering is stable across applies.
func collectPrivilegedServices(project *composetypes.Project) []privilegedService {
	if project == nil {
		return nil
	}
	var out []privilegedService
	for _, svc := range project.Services {
		var fields []string
		if svc.Privileged {
			fields = append(fields, "privileged")
		}
		if len(svc.SecurityOpt) > 0 {
			fields = append(fields, "security_opt")
		}
		if len(fields) == 0 {
			continue
		}
		out = append(out, privilegedService{Name: svc.Name, Fields: fields})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// checkPrivilegedAdmission rejects the apply when any of the supplied
// privileged services would land but the calling identity's token does not
// carry `allows_privileged=true`. Local unix-socket callers (admission's
// LocalIdentity) bypass the check — the socket's filesystem permissions
// already gate operator-class access (see admission.go).
//
// Returns a `codes.PermissionDenied` status whose detail message names
// the first offending service and the exact field(s) that tripped the gate,
// so an operator scanning logs can fix the manifest or re-issue the token
// without reading further.
func checkPrivilegedAdmission(st *state.State, identity string, svcs []privilegedService) error {
	if len(svcs) == 0 {
		return nil
	}
	if identity == admission.LocalIdentity {
		return nil
	}
	if identity == "" {
		// Defensive: a TCP path that reaches here without an identity
		// means the admission interceptor was bypassed. Refuse rather
		// than admit silently — same posture as token_invalid.
		return errorStatus(codes.PermissionDenied, "privilege_denied",
			"privileged workload requires authenticated identity")
	}
	tok, ok := st.Tokens.Get(identity)
	if !ok {
		return errorStatus(codes.PermissionDenied, "privilege_denied",
			fmt.Sprintf("token %q not found; cannot admit privileged workload", identity))
	}
	if !tok.GetAllowsPrivileged() {
		first := svcs[0]
		return errorStatus(codes.PermissionDenied, "privilege_denied",
			fmt.Sprintf("token %q lacks allows_privileged; service %q uses %s",
				identity, first.Name, joinFields(first.Fields)))
	}
	return nil
}

// emitPrivilegedAdmittedAudit raft-Applies one AuditAppend per admitted
// privileged service. Best-effort: a transient raft failure is logged and
// dropped, mirroring challenge.emitAudit so an audit-store hiccup never
// fails an otherwise valid apply.
func emitPrivilegedAdmittedAudit(r *raftnode.Node, identity, deployment string, p privilegedService) {
	if r == nil {
		return
	}
	cmd := &pb.Command{
		Identity: identity,
		Ts:       timestamppb.New(time.Now()),
		Payload: &pb.Command_AuditAppend{AuditAppend: &pb.AuditAppend{
			Event: &pb.AuditEvent{
				Type:     pb.AuditEventType_AUDIT_EVENT_TYPE_PRIVILEGED_WORKLOAD_ADMITTED,
				Identity: identity,
				Payload: map[string]string{
					"deployment": deployment,
					"service":    p.Name,
					"identity":   identity,
					"fields":     joinFields(p.Fields),
				},
			},
		}},
	}
	if err := applyRaftCommand(r, cmd); err != nil {
		slog.Default().Warn("privileged_workload_admitted audit emit failed",
			"deployment", deployment, "service", p.Name, "error", err)
	}
}

// joinFields renders the list of gated fields as a stable comma-separated
// string for the rejection message and audit payload. Input is expected
// sorted; the helper exists only to keep callers from open-coding strings.Join.
func joinFields(fields []string) string {
	switch len(fields) {
	case 0:
		return ""
	case 1:
		return fields[0]
	default:
		out := fields[0]
		for _, f := range fields[1:] {
			out += "," + f
		}
		return out
	}
}
