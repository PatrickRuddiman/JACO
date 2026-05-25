package state_test

import (
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newStore(t *testing.T) (*state.State, *watch.Registry) {
	t.Helper()
	brokers := watch.NewRegistry()
	return state.New(brokers), brokers
}

func TestNodesApplyGetList(t *testing.T) {
	s, _ := newStore(t)

	n1 := &pb.Node{Hostname: "node-a", Address: "10.0.0.1:7000"}
	if added := s.Nodes.Apply(n1, 1); !added {
		t.Errorf("first Apply: added=false, want true")
	}

	got, ok := s.Nodes.Get("node-a")
	if !ok {
		t.Fatalf("Get(node-a) returned ok=false")
	}
	if got.GetAddress() != "10.0.0.1:7000" {
		t.Errorf("Get(node-a).Address = %q, want 10.0.0.1:7000", got.GetAddress())
	}

	// Update — Apply should report added=false on second call.
	n1updated := &pb.Node{Hostname: "node-a", Address: "10.0.0.1:7001"}
	if added := s.Nodes.Apply(n1updated, 2); added {
		t.Errorf("second Apply: added=true, want false")
	}

	if got, _ := s.Nodes.Get("node-a"); got.GetAddress() != "10.0.0.1:7001" {
		t.Errorf("after update Address = %q, want 10.0.0.1:7001", got.GetAddress())
	}

	s.Nodes.Apply(&pb.Node{Hostname: "node-b", Address: "10.0.0.2:7000"}, 3)
	if got := s.Nodes.Len(); got != 2 {
		t.Errorf("Len() = %d, want 2", got)
	}
	if got := s.Nodes.List(); len(got) != 2 {
		t.Errorf("List() len = %d, want 2", len(got))
	}
}

func TestStoreGetReturnsDefensiveCopy(t *testing.T) {
	s, _ := newStore(t)
	s.Nodes.Apply(&pb.Node{Hostname: "node-a", Address: "10.0.0.1:7000"}, 1)

	got, _ := s.Nodes.Get("node-a")
	got.Address = "mutated"

	again, _ := s.Nodes.Get("node-a")
	if again.GetAddress() != "10.0.0.1:7000" {
		t.Errorf("store was mutated through Get return: got %q", again.GetAddress())
	}
}

func TestStoreRemove(t *testing.T) {
	s, _ := newStore(t)
	s.Nodes.Apply(&pb.Node{Hostname: "node-a"}, 1)
	if ok := s.Nodes.Remove("node-a", 2); !ok {
		t.Errorf("Remove(existing) returned false")
	}
	if ok := s.Nodes.Remove("node-a", 3); ok {
		t.Errorf("Remove(missing) returned true")
	}
	if _, ok := s.Nodes.Get("node-a"); ok {
		t.Errorf("Get after Remove returned ok=true")
	}
}

func TestApplyEmitsWatchEvents(t *testing.T) {
	s, brokers := newStore(t)
	sub := brokers.Nodes.Subscribe()
	t.Cleanup(sub.Cancel)

	s.Nodes.Apply(&pb.Node{Hostname: "node-a", Address: "10.0.0.1:7000"}, 1)
	s.Nodes.Apply(&pb.Node{Hostname: "node-a", Address: "10.0.0.1:7001"}, 2)
	s.Nodes.Remove("node-a", 3)

	for i, want := range []watch.Kind{watch.KindAdded, watch.KindUpdated, watch.KindRemoved} {
		ev := <-sub.Events()
		if ev.Kind != want {
			t.Errorf("event %d: kind=%v, want %v", i, ev.Kind, want)
		}
		if ev.RaftIndex != uint64(i+1) {
			t.Errorf("event %d: RaftIndex=%d, want %d", i, ev.RaftIndex, i+1)
		}
	}
}

func TestSubnetCompositeKey(t *testing.T) {
	s, _ := newStore(t)
	// Same (deployment, network) on two hosts must key distinctly.
	s.Subnets.Apply(&pb.Subnet{Deployment: "front", Network: "_default", Cidr: "10.42.0.0/24", Host: "host-a"}, 1)
	s.Subnets.Apply(&pb.Subnet{Deployment: "front", Network: "_default", Cidr: "10.42.1.0/24", Host: "host-b"}, 2)
	s.Subnets.Apply(&pb.Subnet{Deployment: "back", Network: "_default", Cidr: "10.42.2.0/24", Host: "host-a"}, 3)

	if got := s.Subnets.Len(); got != 3 {
		t.Errorf("Len() = %d, want 3", got)
	}

	got, ok := s.Subnets.Get(state.SubnetKey("front", "_default", "host-a"))
	if !ok || got.GetCidr() != "10.42.0.0/24" {
		t.Errorf("front/_default/host-a: ok=%v cidr=%q", ok, got.GetCidr())
	}
	got, ok = s.Subnets.Get(state.SubnetKey("front", "_default", "host-b"))
	if !ok || got.GetCidr() != "10.42.1.0/24" {
		t.Errorf("front/_default/host-b: ok=%v cidr=%q", ok, got.GetCidr())
	}
	if _, ok := s.Subnets.Get(state.SubnetKey("front", "_default", "host-c")); ok {
		t.Errorf("front/_default/host-c should not exist")
	}
}

func TestJoinTokenHexKey(t *testing.T) {
	s, _ := newStore(t)
	hashA := []byte{0xde, 0xad, 0xbe, 0xef}
	hashB := []byte{0xfe, 0xed, 0xfa, 0xce}

	s.JoinTokens.Apply(&pb.JoinToken{HashedSecret: hashA}, 1)
	s.JoinTokens.Apply(&pb.JoinToken{HashedSecret: hashB}, 2)

	if _, ok := s.JoinTokens.Get("deadbeef"); !ok {
		t.Errorf("Get(deadbeef): missing")
	}
	if _, ok := s.JoinTokens.Get("feedface"); !ok {
		t.Errorf("Get(feedface): missing")
	}
}

func TestAuditEventsAppendAndList(t *testing.T) {
	s, brokers := newStore(t)
	sub := brokers.AuditEvents.Subscribe()
	t.Cleanup(sub.Cancel)

	s.AuditEvents.Append(&pb.AuditEvent{RaftIndex: 1, Type: pb.AuditEventType_AUDIT_EVENT_TYPE_NODE_JOIN, Identity: "operator"})
	s.AuditEvents.Append(&pb.AuditEvent{RaftIndex: 2, Type: pb.AuditEventType_AUDIT_EVENT_TYPE_APPLY, Identity: "alice"})

	if got := s.AuditEvents.Len(); got != 2 {
		t.Errorf("Len() = %d, want 2", got)
	}
	if got := s.AuditEvents.List(); len(got) != 2 || got[0].GetIdentity() != "operator" || got[1].GetIdentity() != "alice" {
		t.Errorf("List() unexpected: %+v", got)
	}

	for i := 0; i < 2; i++ {
		ev := <-sub.Events()
		if ev.Kind != watch.KindAdded {
			t.Errorf("audit event %d kind=%v, want Added", i, ev.Kind)
		}
	}
}

func TestTCPRouteKeyedByPublishedPort(t *testing.T) {
	s, _ := newStore(t)

	s.TCPRoutes.Apply(&pb.TCPRoute{PublishedPort: 5432, Deployment: "db", Service: "pg", ContainerPort: 5432}, 1)
	s.TCPRoutes.Apply(&pb.TCPRoute{PublishedPort: 6379, Deployment: "cache", Service: "redis", ContainerPort: 6379}, 2)

	if got := s.TCPRoutes.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}

	got, ok := s.TCPRoutes.Get(state.TCPRouteKey(5432))
	if !ok || got.GetDeployment() != "db" || got.GetContainerPort() != 5432 {
		t.Errorf("Get(5432): ok=%v deployment=%q container=%d", ok, got.GetDeployment(), got.GetContainerPort())
	}

	if ok := s.TCPRoutes.Remove(state.TCPRouteKey(5432), 3); !ok {
		t.Errorf("Remove(5432) returned false")
	}
	if _, ok := s.TCPRoutes.Get(state.TCPRouteKey(5432)); ok {
		t.Errorf("Get after Remove returned ok=true")
	}
}

func TestTCPRouteEmitsWatchEvents(t *testing.T) {
	s, brokers := newStore(t)
	sub := brokers.TCPRoutes.Subscribe()
	t.Cleanup(sub.Cancel)

	s.TCPRoutes.Apply(&pb.TCPRoute{PublishedPort: 5432, Deployment: "db"}, 1)
	s.TCPRoutes.Apply(&pb.TCPRoute{PublishedPort: 5432, Deployment: "db", ContainerPort: 5433}, 2)
	s.TCPRoutes.Remove(state.TCPRouteKey(5432), 3)

	for i, want := range []watch.Kind{watch.KindAdded, watch.KindUpdated, watch.KindRemoved} {
		ev := <-sub.Events()
		if ev.Kind != want {
			t.Errorf("event %d: kind=%v, want %v", i, ev.Kind, want)
		}
	}
}

func TestClusterSingleton(t *testing.T) {
	s, brokers := newStore(t)
	sub := brokers.Cluster.Subscribe()
	t.Cleanup(sub.Cancel)

	if got := s.Cluster.Get(); got != nil {
		t.Errorf("Get on empty cluster: got %+v, want nil", got)
	}

	s.Cluster.Set(&pb.ClusterMeta{ClusterId: "abc"}, 1)
	if got := s.Cluster.Get(); got.GetClusterId() != "abc" {
		t.Errorf("Get after Set: cluster_id=%q", got.GetClusterId())
	}

	s.Cluster.Set(&pb.ClusterMeta{ClusterId: "def"}, 2)
	if got := s.Cluster.Get(); got.GetClusterId() != "def" {
		t.Errorf("Get after second Set: cluster_id=%q", got.GetClusterId())
	}

	for i, want := range []watch.Kind{watch.KindAdded, watch.KindUpdated} {
		ev := <-sub.Events()
		if ev.Kind != want {
			t.Errorf("event %d kind=%v, want %v", i, ev.Kind, want)
		}
	}
}
