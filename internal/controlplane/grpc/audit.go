package grpcsrv

import (
	"strings"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// auditServer implements jaco.v1.Audit. Reads from local state.AuditEvents for
// history; subscribes to the AuditEvents broker for follow mode.
type auditServer struct {
	pb.UnimplementedAuditServer
	state   *state.State
	brokers *watch.Registry
}

// Query streams audit events. Without follow: send everything matching the
// filter, then close. With follow: send historical, then subscribe to the
// AuditEvents broker and stream new matches as they land. Cancellation comes
// from the client (stream.Context().Done()).
func (a *auditServer) Query(req *pb.AuditQueryRequest, stream pb.Audit_QueryServer) error {
	matches := buildMatcher(req)

	// Order matters: when follow=true we subscribe BEFORE reading historical
	// so the broker buffer captures any events that land mid-snapshot;
	// dedupe by raft_index.
	var sub *watch.Subscription[*pb.AuditEvent]
	if req.GetFollow() {
		sub = a.brokers.AuditEvents.Subscribe()
		defer sub.Cancel()
	}

	historical := a.state.AuditEvents.List()
	var lastIdx uint64
	for _, ev := range historical {
		if ev.GetRaftIndex() > lastIdx {
			lastIdx = ev.GetRaftIndex()
		}
		if matches(ev) {
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}

	if sub == nil {
		return nil
	}

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case bev, ok := <-sub.Events():
			if !ok {
				return nil
			}
			switch bev.Kind {
			case watch.KindAdded:
				ev := bev.After
				if ev == nil || ev.GetRaftIndex() <= lastIdx {
					continue
				}
				lastIdx = ev.GetRaftIndex()
				if matches(ev) {
					if err := stream.Send(ev); err != nil {
						return err
					}
				}
			case watch.KindResync:
				// Broker overflow — refetch from state and forward anything
				// past lastIdx.
				for _, ev := range a.state.AuditEvents.List() {
					if ev.GetRaftIndex() <= lastIdx {
						continue
					}
					lastIdx = ev.GetRaftIndex()
					if matches(ev) {
						if err := stream.Send(ev); err != nil {
							return err
						}
					}
				}
			}
		}
	}
}

// buildMatcher compiles req into a predicate. Empty type list ⇒ all types;
// nil since/until ⇒ no bound on that side.
func buildMatcher(req *pb.AuditQueryRequest) func(*pb.AuditEvent) bool {
	var typeFilter map[pb.AuditEventType]bool
	if len(req.GetTypes()) > 0 {
		typeFilter = make(map[pb.AuditEventType]bool, len(req.GetTypes()))
		for _, t := range req.GetTypes() {
			typeFilter[t] = true
		}
	}
	var sinceT, untilT time.Time
	if s := req.GetSince(); s != nil {
		sinceT = s.AsTime()
	}
	if u := req.GetUntil(); u != nil {
		untilT = u.AsTime()
	}
	return func(ev *pb.AuditEvent) bool {
		if typeFilter != nil && !typeFilter[ev.GetType()] {
			return false
		}
		if !sinceT.IsZero() {
			if t := ev.GetTs(); t == nil || t.AsTime().Before(sinceT) {
				return false
			}
		}
		if !untilT.IsZero() {
			if t := ev.GetTs(); t != nil && t.AsTime().After(untilT) {
				return false
			}
		}
		return true
	}
}

// AuditTypeToString returns the short human name for an audit type — the
// suffix after `AUDIT_EVENT_TYPE_`, lowercased. E.g. APPLY → "apply".
// UNSPECIFIED → "" (never written by the FSM in practice).
func AuditTypeToString(t pb.AuditEventType) string {
	s := t.String()
	s = strings.TrimPrefix(s, "AUDIT_EVENT_TYPE_")
	if s == "UNSPECIFIED" {
		return ""
	}
	return strings.ToLower(s)
}

// ParseAuditType is the inverse of AuditTypeToString. Accepts both short
// ("apply") and full ("AUDIT_EVENT_TYPE_APPLY") forms, case-insensitive.
func ParseAuditType(s string) (pb.AuditEventType, bool) {
	up := strings.ToUpper(strings.TrimSpace(s))
	if up == "" {
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, false
	}
	if !strings.HasPrefix(up, "AUDIT_EVENT_TYPE_") {
		up = "AUDIT_EVENT_TYPE_" + up
	}
	v, ok := pb.AuditEventType_value[up]
	if !ok {
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, false
	}
	return pb.AuditEventType(v), true
}
