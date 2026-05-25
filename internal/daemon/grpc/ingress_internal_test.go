package grpc

import (
	"bytes"
	"io"
	"log/slog"
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

// TestIngressBuilder_EmitsTCPRoute: a TCPRoute with a healthy replica produces
// a layer4 server in the built config. (No bind-probe — caddy-l4 owns the
// listener and re-binding its own port on reload is idempotent.)
func TestIngressBuilder_EmitsTCPRoute(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	seedHealthyTCPService(t, st, "data", "db", 5432)

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := ingressBuilder(st, ingressACMEOpts{}, discard)()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !bytes.Contains(cfg, []byte("tcp_5432")) {
		t.Errorf("built config missing tcp_5432 server:\n%s", cfg)
	}
}

// TestShouldLoad guards the startup-vs-teardown gate: route-less configs are
// skipped only before caddy first starts; once running, they MUST load so a
// deleted route's listeners are torn down.
func TestShouldLoad(t *testing.T) {
	fallback := []byte(`{"apps":{"http":{"servers":{"jaco":{"routes":[{"handle":[{"handler":"static_response"}]}]}}}}}`)
	httpCfg := []byte(`{"apps":{"http":{"servers":{"jaco":{"routes":[{"handle":[{"handler":"reverse_proxy"}]}]}}}}}`)
	l4Cfg := []byte(`{"apps":{"layer4":{"servers":{"tcp_5432":{"listen":[":5432"]}}}}}`)
	cases := []struct {
		name    string
		started bool
		cfg     []byte
		want    bool
	}{
		{"startup + route-less -> skip", false, fallback, false},
		{"startup + http -> load", false, httpCfg, true},
		{"startup + layer4 -> load", false, l4Cfg, true},
		{"running + route-less -> load (teardown)", true, fallback, true},
		{"running + layer4 -> load", true, l4Cfg, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldLoad(tc.started, tc.cfg); got != tc.want {
				t.Errorf("shouldLoad(%v, ...) = %v, want %v", tc.started, got, tc.want)
			}
		})
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
