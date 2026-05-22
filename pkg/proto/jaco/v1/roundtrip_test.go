package jacov1_test

import (
	"testing"

	jacov1 "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/proto"
)

func TestCommandRoundTripNodeJoin(t *testing.T) {
	cmd := &jacov1.Command{
		ClusterId: "cluster-x",
		RaftIndex: 42,
		Identity:  "operator",
		Payload: &jacov1.Command_NodeJoin{NodeJoin: &jacov1.NodeJoin{
			Hostname: "node-a",
			Address:  "127.0.0.1:7000",
		}},
	}

	bytes, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded jacov1.Command
	if err := proto.Unmarshal(bytes, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := decoded.GetClusterId(); got != "cluster-x" {
		t.Errorf("cluster_id: got %q, want cluster-x", got)
	}
	if got := decoded.GetRaftIndex(); got != 42 {
		t.Errorf("raft_index: got %d, want 42", got)
	}
	nj := decoded.GetNodeJoin()
	if nj == nil {
		t.Fatalf("node_join: payload missing")
	}
	if nj.GetHostname() != "node-a" {
		t.Errorf("node_join.hostname: got %q", nj.GetHostname())
	}
}

func TestErrorEnvelope(t *testing.T) {
	e := &jacov1.Error{
		Code:    "token_invalid",
		Message: "no such token",
		Details: map[string]string{"identity": "alice"},
	}
	bytes, err := proto.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded jacov1.Error
	if err := proto.Unmarshal(bytes, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.GetCode() != "token_invalid" || decoded.GetMessage() != "no such token" {
		t.Errorf("decoded mismatch: %+v", &decoded)
	}
	if got := decoded.GetDetails()["identity"]; got != "alice" {
		t.Errorf("details[identity]: got %q", got)
	}
}

func TestSubscribeEventOneof(t *testing.T) {
	ev := &jacov1.SubscribeEvent{
		Payload: &jacov1.SubscribeEvent_Deployment{Deployment: &jacov1.DeploymentEvent{
			Kind:      jacov1.EventKind_EVENT_KIND_UPDATED,
			RaftIndex: 99,
			After:     &jacov1.Deployment{Name: "sample", AppliedRevision: 2},
		}},
	}
	bytes, err := proto.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded jacov1.SubscribeEvent
	if err := proto.Unmarshal(bytes, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dep := decoded.GetDeployment()
	if dep == nil {
		t.Fatalf("deployment payload missing")
	}
	if dep.GetKind() != jacov1.EventKind_EVENT_KIND_UPDATED {
		t.Errorf("kind: got %v", dep.GetKind())
	}
	if dep.GetAfter().GetName() != "sample" {
		t.Errorf("after.name: got %q", dep.GetAfter().GetName())
	}
}
