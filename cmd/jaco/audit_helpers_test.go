package main

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestContextForStream_FollowReturnsCancelable — follow=true gives the
// caller a context.CancelFunc that cancels on demand and has no
// deadline.
func TestContextForStream_FollowReturnsCancelable(t *testing.T) {
	ctx, cancel := contextForStream(true)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Errorf("follow=true context should not have a deadline")
	}
	cancel()
	select {
	case <-ctx.Done():
	default:
		t.Errorf("cancel() did not close the context")
	}
}

// TestContextForStream_NonFollowReturns30sDeadline — follow=false sets
// a 30s timeout so the CLI doesn't hang on a quiet stream.
func TestContextForStream_NonFollowReturns30sDeadline(t *testing.T) {
	ctx, cancel := contextForStream(false)
	defer cancel()
	d, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("follow=false should set a deadline")
	}
	remaining := time.Until(d)
	if remaining < 25*time.Second || remaining > 31*time.Second {
		t.Errorf("deadline = %v from now, want ~30s", remaining)
	}
}

// TestEventToJSON_RoundTripsFields — the conversion preserves the
// fields the CLI prints.
func TestEventToJSON_RoundTripsFields(t *testing.T) {
	ts := timestamppb.New(time.Unix(1700000000, 0).UTC())
	ev := &pb.AuditEvent{
		Type:      pb.AuditEventType_AUDIT_EVENT_TYPE_NODE_JOIN,
		Identity:  "alice",
		Ts:        ts,
		RaftIndex: 42,
		Payload:   map[string]string{"hostname": "node-a"},
	}
	got := eventToJSON(ev)
	if got.Type == "" {
		t.Errorf("Type empty; expected NODE_JOIN-derived string")
	}
	if got.Identity != "alice" {
		t.Errorf("Identity = %q, want alice", got.Identity)
	}
	if got.RaftIndex != 42 {
		t.Errorf("RaftIndex = %d, want 42", got.RaftIndex)
	}
	if got.Payload["hostname"] != "node-a" {
		t.Errorf("Payload[hostname] = %q", got.Payload["hostname"])
	}
	if !strings.HasPrefix(got.Ts, "2023-") {
		t.Errorf("Ts = %q, want RFC3339 from 2023", got.Ts)
	}
}

// TestEventToJSON_NilTimestampOmitsField — when Ts is nil, the JSON
// payload's Ts is empty (the `omitempty` tag suppresses it).
func TestEventToJSON_NilTimestampOmitsField(t *testing.T) {
	ev := &pb.AuditEvent{Type: pb.AuditEventType_AUDIT_EVENT_TYPE_DELETE}
	got := eventToJSON(ev)
	if got.Ts != "" {
		t.Errorf("Ts = %q, want empty when ev.Ts is nil", got.Ts)
	}
}
