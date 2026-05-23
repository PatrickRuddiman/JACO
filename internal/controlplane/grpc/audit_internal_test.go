package grpcsrv

import (
	"testing"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestAuditTypeToString_Roundtrip — exercises the short-name path:
// APPLY → "apply"; UNSPECIFIED → "".
func TestAuditTypeToString_Roundtrip(t *testing.T) {
	cases := []struct {
		in   pb.AuditEventType
		want string
	}{
		{pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, ""},
		{pb.AuditEventType_AUDIT_EVENT_TYPE_APPLY, "apply"},
		{pb.AuditEventType_AUDIT_EVENT_TYPE_DELETE, "delete"},
		{pb.AuditEventType_AUDIT_EVENT_TYPE_NODE_JOIN, "node_join"},
		{pb.AuditEventType_AUDIT_EVENT_TYPE_TOKEN_ISSUE, "token_issue"},
		{pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED, "certificate_renewed"},
	}
	for _, c := range cases {
		if got := AuditTypeToString(c.in); got != c.want {
			t.Errorf("AuditTypeToString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseAuditType_AcceptsShortAndFull — the inverse mapping.
func TestParseAuditType_AcceptsShortAndFull(t *testing.T) {
	cases := []struct {
		in   string
		want pb.AuditEventType
		ok   bool
	}{
		{"apply", pb.AuditEventType_AUDIT_EVENT_TYPE_APPLY, true},
		{"APPLY", pb.AuditEventType_AUDIT_EVENT_TYPE_APPLY, true},
		{"AUDIT_EVENT_TYPE_APPLY", pb.AuditEventType_AUDIT_EVENT_TYPE_APPLY, true},
		{"  apply  ", pb.AuditEventType_AUDIT_EVENT_TYPE_APPLY, true},
		{"node_join", pb.AuditEventType_AUDIT_EVENT_TYPE_NODE_JOIN, true},
		{"", pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, false},
		{"unknown_kind", pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, false},
	}
	for _, c := range cases {
		got, ok := ParseAuditType(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseAuditType(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
