package bridge_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	dnet "github.com/docker/docker/api/types/network"

	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
)

// fakeDocker partial-impl: only NetworkList / NetworkCreate / NetworkRemove.
type fakeDocker struct {
	dockerx.Docker
	mu       sync.Mutex
	networks map[string]*dnet.Summary
	idSeq    int
}

func newFakeDocker() *fakeDocker { return &fakeDocker{networks: map[string]*dnet.Summary{}} }

func (f *fakeDocker) NetworkList(_ context.Context, opts dnet.ListOptions) ([]dnet.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	labelFilters := opts.Filters.Get("label")
	nameFilters := opts.Filters.Get("name")
	var out []dnet.Summary
	for _, n := range f.networks {
		ok := true
		for _, lf := range labelFilters {
			parts := strings.SplitN(lf, "=", 2)
			if len(parts) != 2 || n.Labels[parts[0]] != parts[1] {
				ok = false
				break
			}
		}
		for _, nf := range nameFilters {
			if n.Name != nf {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, *n)
		}
	}
	return out, nil
}

func (f *fakeDocker) NetworkCreate(_ context.Context, name string, opts dnet.CreateOptions) (dnet.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idSeq++
	id := "n-" + name
	labels := map[string]string{}
	for k, v := range opts.Labels {
		labels[k] = v
	}
	f.networks[id] = &dnet.Summary{
		ID: id, Name: name, Driver: opts.Driver, Labels: labels, Options: opts.Options,
		IPAM: dnet.IPAM{Driver: opts.IPAM.Driver, Config: opts.IPAM.Config},
	}
	return dnet.CreateResponse{ID: id}, nil
}

func (f *fakeDocker) NetworkRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.networks, id)
	return nil
}

func TestDockerNetworkName_UsesUnderscoreSeparators(t *testing.T) {
	if got := bridge.DockerNetworkName("sample", "frontend"); got != "jaco_sample_frontend" {
		t.Errorf("DockerNetworkName = %q, want jaco_sample_frontend", got)
	}
	// compose's "default" maps to JACO's "_default" at the boundary.
	if got := bridge.DockerNetworkName("sample", "default"); got != "jaco_sample__default" {
		t.Errorf("DockerNetworkName(default) = %q, want jaco_sample__default", got)
	}
}

func TestLinuxBridgeName_FitsKernel15CharLimit(t *testing.T) {
	cases := [][2]string{
		{"sample", "frontend"},
		{"very-long-deployment-name-that-would-overflow", "metrics-collection-backbone"},
		{"x", "y"},
		{"sample", "default"},
	}
	for _, c := range cases {
		got := bridge.LinuxBridgeName(c[0], c[1])
		if len(got) > 15 {
			t.Errorf("LinuxBridgeName(%q,%q) = %q (%d chars); must be <= 15", c[0], c[1], got, len(got))
		}
		if !strings.HasPrefix(got, "jaco-") {
			t.Errorf("LinuxBridgeName(%q,%q) = %q; want jaco- prefix", c[0], c[1], got)
		}
	}
}

func TestLinuxBridgeName_IsDeterministic(t *testing.T) {
	a := bridge.LinuxBridgeName("sample", "frontend")
	for i := 0; i < 100; i++ {
		if bridge.LinuxBridgeName("sample", "frontend") != a {
			t.Fatalf("LinuxBridgeName not deterministic")
		}
	}
}

func TestGatewayIP_FirstUsableAddressOfSlash24(t *testing.T) {
	cases := []struct {
		cidr, want string
	}{
		{"10.244.0.0/24", "10.244.0.1"},
		{"10.244.5.0/24", "10.244.5.1"},
		{"192.168.1.0/24", "192.168.1.1"},
	}
	for _, c := range cases {
		got, err := bridge.GatewayIP(c.cidr)
		if err != nil {
			t.Fatalf("GatewayIP(%q): %v", c.cidr, err)
		}
		if got != c.want {
			t.Errorf("GatewayIP(%q) = %q, want %q", c.cidr, got, c.want)
		}
	}
}

func TestGatewayIP_RejectsGarbage(t *testing.T) {
	if _, err := bridge.GatewayIP("not-a-cidr"); err == nil {
		t.Errorf("expected error on garbage")
	}
}

func TestEnsure_CreatesNetworkWithLabelsAndIPAM(t *testing.T) {
	d := newFakeDocker()
	name, err := bridge.Ensure(context.Background(), d, "sample", "frontend", "10.244.5.0/24", "cluster-x")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if name != "jaco_sample_frontend" {
		t.Errorf("returned name = %q", name)
	}
	if len(d.networks) != 1 {
		t.Fatalf("network count = %d, want 1", len(d.networks))
	}
	for _, n := range d.networks {
		for k, want := range map[string]string{
			"jaco.cluster_id": "cluster-x",
			"jaco.deployment": "sample",
			"jaco.network":    "frontend",
			"jaco.subnet":     "10.244.5.0/24",
		} {
			if got := n.Labels[k]; got != want {
				t.Errorf("label %q = %q, want %q", k, got, want)
			}
		}
		if got := n.Options["com.docker.network.bridge.name"]; got != bridge.LinuxBridgeName("sample", "frontend") {
			t.Errorf("bridge name option = %q", got)
		}
		if got := n.Options["com.docker.network.driver.mtu"]; got != "1420" {
			t.Errorf("mtu option = %q, want 1420", got)
		}
		if len(n.IPAM.Config) != 1 {
			t.Fatalf("IPAM config len = %d", len(n.IPAM.Config))
		}
		if got := n.IPAM.Config[0].Subnet; got != "10.244.5.0/24" {
			t.Errorf("IPAM subnet = %q", got)
		}
		if got := n.IPAM.Config[0].Gateway; got != "10.244.5.1" {
			t.Errorf("IPAM gateway = %q", got)
		}
	}
}

func TestEnsure_IsIdempotent(t *testing.T) {
	d := newFakeDocker()
	if _, err := bridge.Ensure(context.Background(), d, "sample", "frontend", "10.244.5.0/24", "cluster-x"); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Ensure(context.Background(), d, "sample", "frontend", "10.244.5.0/24", "cluster-x"); err != nil {
		t.Fatal(err)
	}
	if len(d.networks) != 1 {
		t.Errorf("second Ensure created a new network; want idempotent. count = %d", len(d.networks))
	}
}

func TestEnsure_RejectsEmptyArgs(t *testing.T) {
	d := newFakeDocker()
	if _, err := bridge.Ensure(context.Background(), d, "", "frontend", "10.244.5.0/24", "cluster-x"); err == nil {
		t.Errorf("empty deployment accepted")
	}
	if _, err := bridge.Ensure(context.Background(), d, "sample", "", "10.244.5.0/24", "cluster-x"); err == nil {
		t.Errorf("empty network accepted")
	}
	if _, err := bridge.Ensure(context.Background(), d, "sample", "frontend", "", "cluster-x"); err == nil {
		t.Errorf("empty cidr accepted")
	}
	if _, err := bridge.Ensure(context.Background(), d, "sample", "frontend", "10.244.5.0/24", ""); err == nil {
		t.Errorf("empty clusterID accepted")
	}
}

func TestTeardown_RemovesNetwork(t *testing.T) {
	d := newFakeDocker()
	if _, err := bridge.Ensure(context.Background(), d, "sample", "frontend", "10.244.5.0/24", "cluster-x"); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Teardown(context.Background(), d, "sample", "frontend"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(d.networks) != 0 {
		t.Errorf("network not removed; count = %d", len(d.networks))
	}
}

func TestTeardown_NoOpWhenMissing(t *testing.T) {
	d := newFakeDocker()
	if err := bridge.Teardown(context.Background(), d, "ghost", "frontend"); err != nil {
		t.Errorf("Teardown on missing: %v", err)
	}
}
