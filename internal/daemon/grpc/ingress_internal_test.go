package grpc

import (
	"bytes"
	"fmt"
	"net"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// seedHealthyTCPService wires the minimal state for one healthy replica of
// (deployment, service) on the default network with a known overlay IP, plus
// a TCPRoute publishing port on container 5432.
func seedHealthyTCPService(t *testing.T, st *state.State, deployment, service string, port int) {
	t.Helper()
	st.Deployments.Apply(&pb.Deployment{
		Name:     deployment,
		Services: []*pb.ServiceSpec{{Name: service, Networks: []string{"_default"}}},
	}, 1)
	id := deployment + "-" + service + "-0"
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{Id: id, Deployment: deployment, Service: service}, 2)
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{
		Id:           id,
		State:        pb.ReplicaState_REPLICA_STATE_RUNNING,
		LastHealthAt: timestamppb.Now(),
		Details:      map[string]string{"ip." + bridge.DockerNetworkName(deployment, "_default"): "10.244.7.2"},
	}, 3)
	st.TCPRoutes.Apply(&pb.TCPRoute{PublishedPort: int32(port), Deployment: deployment, Service: service, ContainerPort: 5432}, 4)
}

// TestIngressBuilder_DropsUnbindableTCPPort: a published port already held by
// another listener on this node is skipped from the built config (so it can't
// fail the atomic caddy.Load), and reappears once the port is free.
func TestIngressBuilder_DropsUnbindableTCPPort(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)

	// Grab a free ephemeral port and hold it, then publish that exact port.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	seedHealthyTCPService(t, st, "data", "db", port)

	build := ingressBuilder(st, "")

	cfg, err := build()
	if err != nil {
		t.Fatalf("build (port occupied): %v", err)
	}
	marker := []byte(fmt.Sprintf("tcp_%d", port))
	if bytes.Contains(cfg, marker) {
		t.Errorf("config still contains tcp_%d while the port is occupied:\n%s", port, cfg)
	}

	// Free the port; the route must now be emitted (proving the drop was the
	// bind probe, not a missing upstream).
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	cfg2, err := build()
	if err != nil {
		t.Fatalf("build (port free): %v", err)
	}
	if !bytes.Contains(cfg2, marker) {
		t.Errorf("config missing tcp_%d after the port was freed:\n%s", port, cfg2)
	}
}

func TestConfigHasLoadableRoute(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want bool
	}{
		{"fallback only", `{"apps":{"http":{"servers":{"jaco":{"routes":[{"handle":[{"handler":"static_response"}]}]}}}}}`, false},
		{"http reverse_proxy", `{"apps":{"http":{"servers":{"jaco":{"routes":[{"handle":[{"handler":"reverse_proxy"}]}]}}}}}`, true},
		{"layer4 only", `{"apps":{"layer4":{"servers":{"tcp_5432":{"listen":[":5432"]}}}}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := configHasLoadableRoute([]byte(tc.cfg)); got != tc.want {
				t.Errorf("configHasLoadableRoute = %v, want %v", got, tc.want)
			}
		})
	}
}
