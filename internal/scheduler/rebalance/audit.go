package rebalance

import (
	"strconv"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// emitAudit raft-Applies one AUDIT_EVENT_TYPE_REBALANCE_* entry.
// Best-effort: a failed apply is logged at WARN by the caller and
// never bubbles back into the cycle's hot path — the audit store
// being briefly unavailable should never block a move (mirrors the
// challenge.go emitAudit pattern).
func (r *Rebalancer) emitAudit(t pb.AuditEventType, payload map[string]string) error {
	cmd := &pb.Command{
		Identity: "scheduler/rebalance",
		Ts:       timestamppb.New(r.clock()),
		Payload: &pb.Command_AuditAppend{AuditAppend: &pb.AuditAppend{
			Event: &pb.AuditEvent{Type: t, Payload: payload},
		}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	return r.apply(data)
}

// auditPayload returns the shared payload shape ADR 0002 documents
// for every rebalance event (MOVED, SKIPPED). Reason is optional —
// SkipNone elides the key entirely.
func auditPayload(c *MoveCandidate, score, relief float64, dominant Dimension, reason SkipReason) map[string]string {
	postSrc, postDst := PostMovePressure(c)
	p := map[string]string{
		"replica_id":          c.Replica.GetId(),
		"deployment":          c.Replica.GetDeployment(),
		"service":             c.Replica.GetService(),
		"src":                 c.Src,
		"dst":                 c.Dst,
		"dominant":            dominant.String(),
		"relief":              formatFloat(relief),
		"score":               formatFloat(score),
		"move_cost":           formatFloat(moveCost),
		"src_pressure_before": formatFloat(Composite(c.SrcPressure)),
		"dst_pressure_before": formatFloat(Composite(c.DstPressure)),
		"src_pressure_after":  formatFloat(Composite(postSrc)),
		"dst_pressure_after":  formatFloat(Composite(postDst)),
	}
	if reason != SkipNone {
		p["reason"] = string(reason)
	}
	return p
}

// formatFloat is a single, deterministic float→string for audit
// payloads. 4 decimal places: enough resolution to compare two
// candidates in the audit log without flooding the value with
// trailing-zero noise.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', 4, 64)
}
