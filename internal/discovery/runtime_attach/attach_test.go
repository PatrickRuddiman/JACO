package runtime_attach_test

import (
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/runtime_attach"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func makeState(dep string, svcs ...*pb.ServiceSpec) *state.State {
	st := state.New(watch.NewRegistry())
	st.Deployments.Apply(&pb.Deployment{Name: dep, Services: svcs}, 1)
	return st
}

func TestBridgesForService_DeclaredNetworks(t *testing.T) {
	st := makeState("sample", &pb.ServiceSpec{
		Name: "web", Networks: []string{"frontend", "backend"},
	})
	got, err := runtime_attach.BridgesForService(st, "sample", "web")
	if err != nil {
		t.Fatalf("BridgesForService: %v", err)
	}
	want := []string{"jaco_sample_frontend", "jaco_sample_backend"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBridgesForService_DefaultsToImplicitDefaultNetwork(t *testing.T) {
	st := makeState("sample", &pb.ServiceSpec{Name: "web"})
	got, _ := runtime_attach.BridgesForService(st, "sample", "web")
	if len(got) != 1 || got[0] != "jaco_sample__default" {
		t.Errorf("got %v, want [jaco_sample__default]", got)
	}
}

func TestBridgesForService_DeploymentMissing(t *testing.T) {
	st := state.New(watch.NewRegistry())
	if _, err := runtime_attach.BridgesForService(st, "ghost", "web"); err == nil {
		t.Errorf("expected error for missing deployment")
	}
}

func TestBridgesForService_ServiceMissing(t *testing.T) {
	st := makeState("sample", &pb.ServiceSpec{Name: "web"})
	if _, err := runtime_attach.BridgesForService(st, "sample", "ghost"); err == nil {
		t.Errorf("expected error for missing service")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
