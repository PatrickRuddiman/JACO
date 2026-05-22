package grpcsrv_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func collectAudit(t *testing.T, c *twoNodeCluster, req *pb.AuditQueryRequest) []*pb.AuditEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = authContext(c.OperatorToken)
	stream, err := c.A.Audit.Query(ctx, req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var out []*pb.AuditEvent
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("stream.Recv: %v", err)
		}
		out = append(out, ev)
	}
}

func TestAudit_QueryFiltersByType(t *testing.T) {
	c := setupTwoNodeCluster(t)
	ctxOp := authContext(c.OperatorToken)

	// Generate a TOKEN_REVOKE event so the filter has something to find.
	if _, err := c.A.Tokens.Revoke(ctxOp, &pb.TokenRevokeRequest{Identity: "ghost"}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// Wait for the audit event to land in state.
	deadline := time.Now().Add(2 * time.Second)
	var revokes []*pb.AuditEvent
	for time.Now().Before(deadline) {
		revokes = collectAudit(t, c, &pb.AuditQueryRequest{
			Types: []pb.AuditEventType{pb.AuditEventType_AUDIT_EVENT_TYPE_TOKEN_REVOKE},
		})
		if len(revokes) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(revokes) == 0 {
		t.Fatalf("no TOKEN_REVOKE events observed")
	}

	// Every returned event must be of the requested type.
	for _, ev := range revokes {
		if ev.GetType() != pb.AuditEventType_AUDIT_EVENT_TYPE_TOKEN_REVOKE {
			t.Errorf("filter leaked: got %v", ev.GetType())
		}
		if ev.GetPayload()["identity"] != "ghost" {
			t.Errorf("unexpected revoke payload: %+v", ev.GetPayload())
		}
		if ev.GetIdentity() != "operator" && ev.GetIdentity() != "bootstrap" {
			// Either is fine — the bootstrap operator token authenticates the
			// call, and admission attaches its identity onto the context.
			t.Logf("audit identity = %q (informational)", ev.GetIdentity())
		}
	}
}

func TestAudit_QuerySinceCutoff(t *testing.T) {
	c := setupTwoNodeCluster(t)
	ctxOp := authContext(c.OperatorToken)

	// Cut-off is now; emit one event strictly after.
	cutoff := time.Now()
	time.Sleep(10 * time.Millisecond)
	if _, err := c.A.Tokens.Revoke(ctxOp, &pb.TokenRevokeRequest{Identity: "post-cutoff"}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// Wait for replication.
	deadline := time.Now().Add(2 * time.Second)
	var post []*pb.AuditEvent
	for time.Now().Before(deadline) {
		post = collectAudit(t, c, &pb.AuditQueryRequest{
			Since: timestamppb.New(cutoff),
		})
		if len(post) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(post) == 0 {
		t.Fatalf("no events after cutoff observed")
	}

	// Every returned event must be strictly newer than the cutoff. The
	// bootstrap NODE_JOIN event lands far before cutoff and must be excluded.
	for _, ev := range post {
		if ts := ev.GetTs(); ts != nil && ts.AsTime().Before(cutoff) {
			t.Errorf("event ts=%v leaked past since=%v: %v", ts.AsTime(), cutoff, ev)
		}
	}
}

func TestAudit_FollowStreamsNewEvents(t *testing.T) {
	c := setupTwoNodeCluster(t)
	ctxOp := authContext(c.OperatorToken)

	streamCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := c.A.Audit.Query(authContext(c.OperatorToken), &pb.AuditQueryRequest{
		Types:  []pb.AuditEventType{pb.AuditEventType_AUDIT_EVENT_TYPE_TOKEN_ISSUE},
		Follow: true,
		Since:  timestamppb.New(time.Now()),
	})
	if err != nil {
		t.Fatalf("Query follow: %v", err)
	}
	_ = streamCtx

	// Emit a token issue event after the stream is open.
	got := make(chan *pb.AuditEvent, 1)
	errs := make(chan error, 1)
	go func() {
		ev, err := stream.Recv()
		if err != nil {
			errs <- err
			return
		}
		got <- ev
	}()

	if _, err := c.A.Tokens.Issue(ctxOp, &pb.TokenIssueRequest{Identity: "future-user"}); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	select {
	case ev := <-got:
		if ev.GetType() != pb.AuditEventType_AUDIT_EVENT_TYPE_TOKEN_ISSUE {
			t.Errorf("event type = %v, want TOKEN_ISSUE", ev.GetType())
		}
		if ev.GetPayload()["identity"] != "future-user" {
			t.Errorf("payload identity = %q, want future-user", ev.GetPayload()["identity"])
		}
	case err := <-errs:
		t.Fatalf("stream.Recv error: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatalf("follow stream did not deliver new event within 3s")
	}
}

func TestAuditTypeStringRoundTrip(t *testing.T) {
	cases := []struct {
		t    pb.AuditEventType
		want string
	}{
		{pb.AuditEventType_AUDIT_EVENT_TYPE_APPLY, "apply"},
		{pb.AuditEventType_AUDIT_EVENT_TYPE_TOKEN_REVOKE, "token_revoke"},
		{pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_RULESET_RECONCILED, "isolation_ruleset_reconciled"},
	}
	for _, c := range cases {
		if got := grpcsrv.AuditTypeToString(c.t); got != c.want {
			t.Errorf("AuditTypeToString(%v) = %q, want %q", c.t, got, c.want)
		}
		got, ok := grpcsrv.ParseAuditType(c.want)
		if !ok || got != c.t {
			t.Errorf("ParseAuditType(%q) = (%v, %v), want (%v, true)", c.want, got, ok, c.t)
		}
	}
	if _, ok := grpcsrv.ParseAuditType("not_a_real_type"); ok {
		t.Errorf("ParseAuditType(unknown) should fail")
	}
}
