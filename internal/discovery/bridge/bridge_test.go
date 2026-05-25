package bridge_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	dnet "github.com/docker/docker/api/types/network"

	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
)

// fakeDocker partial-impl: NetworkList / NetworkCreate / NetworkRemove /
// NetworkInspect, plus the ContainerStop+Remove pair needed by Ensure's
// subnet-mismatch recreate path.
type fakeDocker struct {
	dockerx.Docker
	mu             sync.Mutex
	networks       map[string]*dnet.Summary
	containers     map[string]string // containerID -> networkID it's attached to
	stopped        []string
	removed        []string
	idSeq          int
	containerIDSeq int
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{networks: map[string]*dnet.Summary{}, containers: map[string]string{}}
}

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
	for cid, nid := range f.containers {
		if nid == id {
			delete(f.containers, cid)
		}
	}
	return nil
}

func (f *fakeDocker) NetworkInspect(_ context.Context, id string, _ dnet.InspectOptions) (dnet.Inspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.networks[id]
	if !ok {
		return dnet.Inspect{}, errFakeNotFound
	}
	insp := dnet.Inspect{
		ID:     n.ID,
		Name:   n.Name,
		Driver: n.Driver,
		Labels: n.Labels,
		IPAM:   dnet.IPAM{Driver: n.IPAM.Driver, Config: n.IPAM.Config},
	}
	insp.Containers = map[string]dnet.EndpointResource{}
	for cid, nid := range f.containers {
		if nid == id {
			insp.Containers[cid] = dnet.EndpointResource{Name: cid}
		}
	}
	return insp, nil
}

func (f *fakeDocker) ContainerStop(_ context.Context, id string, _ container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	delete(f.containers, id)
	return nil
}

// attachContainer records a fake container attachment so NetworkInspect
// surfaces it on the next call. Returns the assigned container id.
func (f *fakeDocker) attachContainer(networkID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerIDSeq++
	id := "c-" + networkID + "-" + itoa(f.containerIDSeq)
	f.containers[id] = networkID
	return id
}

// itoa avoids dragging strconv into the imports just for two test calls.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// errFakeNotFound mirrors a docker engine 404 enough for the bridge package to
// surface the failure. We don't inspect its type; only that it's non-nil.
var errFakeNotFound = &fakeError{msg: "network not found"}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

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
	name, err := bridge.Ensure(context.Background(), d, "sample", "frontend", "10.244.5.0/24", "cluster-x", nil)
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
	if _, err := bridge.Ensure(context.Background(), d, "sample", "frontend", "10.244.5.0/24", "cluster-x", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Ensure(context.Background(), d, "sample", "frontend", "10.244.5.0/24", "cluster-x", nil); err != nil {
		t.Fatal(err)
	}
	if len(d.networks) != 1 {
		t.Errorf("second Ensure created a new network; want idempotent. count = %d", len(d.networks))
	}
}

// TestEnsure_RecreatesOnSubnetMismatch covers issue #42: when raft state is
// wiped and the cluster is re-formed in place, the freshly-allocated per-host
// /24 can differ from the existing docker bridge's subnet. Ensure must detect
// the drift, tear down the stale network (and any containers attached to it),
// and recreate with the new CIDR.
func TestEnsure_RecreatesOnSubnetMismatch(t *testing.T) {
	d := newFakeDocker()
	ctx := context.Background()
	// Initial create with the old /24.
	if _, err := bridge.Ensure(ctx, d, "sample", "frontend", "10.244.0.0/24", "cluster-x", nil); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	var oldID string
	for id := range d.networks {
		oldID = id
	}
	// Simulate a container attached to the stale bridge (a leftover from
	// the prior deployment that re-form-in-place doesn't clean up).
	attached := d.attachContainer(oldID)
	removeCallsBefore := len(d.removed)

	// Re-form in place: same (deployment, network, cluster_id) but a NEW /24.
	if _, err := bridge.Ensure(ctx, d, "sample", "frontend", "10.244.5.0/24", "cluster-x", nil); err != nil {
		t.Fatalf("second Ensure (mismatched cidr): %v", err)
	}
	if len(d.networks) != 1 {
		t.Fatalf("expected exactly 1 network after recreate, got %d", len(d.networks))
	}
	var newNet *dnet.Summary
	for _, n := range d.networks {
		newNet = n
	}
	// The fake reuses ID-by-name, so the post-recreate ID equals the pre-
	// recreate ID; what proves the recreate happened is that the IPAM
	// subnet now matches the new CIDR (and the stale attached container was
	// removed during teardown).
	_ = oldID
	if got := newNet.IPAM.Config[0].Subnet; got == "10.244.0.0/24" {
		t.Fatalf("network still carries the stale /24 — recreate did not happen")
	}
	if got := newNet.IPAM.Config[0].Subnet; got != "10.244.5.0/24" {
		t.Errorf("recreated network subnet = %q, want 10.244.5.0/24", got)
	}
	if got := newNet.Labels["jaco.subnet"]; got != "10.244.5.0/24" {
		t.Errorf("recreated network jaco.subnet label = %q, want 10.244.5.0/24", got)
	}
	// Container attached to the stale bridge must have been stopped+removed.
	if len(d.stopped) != 1 || d.stopped[0] != attached {
		t.Errorf("expected attached container %q stopped; got stopped=%v", attached, d.stopped)
	}
	if got := len(d.removed) - removeCallsBefore; got != 1 || d.removed[len(d.removed)-1] != attached {
		t.Errorf("expected attached container %q removed; got removed=%v", attached, d.removed)
	}

	// Third call with the now-current CIDR is a no-op (idempotency preserved).
	prevCount := len(d.networks)
	if _, err := bridge.Ensure(ctx, d, "sample", "frontend", "10.244.5.0/24", "cluster-x", nil); err != nil {
		t.Fatalf("third Ensure (matching cidr): %v", err)
	}
	if len(d.networks) != prevCount {
		t.Errorf("third Ensure created a new network; want idempotent. count = %d", len(d.networks))
	}
}

// TestEnsure_RecreatesOnClusterIDDrift covers the actual issue #42 trigger:
// raft state is wiped and the cluster is re-formed in place, so the new
// clusterID differs from the stale bridge's jaco.cluster_id label. Ensure
// must still claim the bridge by name (it's JACO-owned per the jaco.*
// labels) and recreate it under the new cluster_id.
func TestEnsure_RecreatesOnClusterIDDrift(t *testing.T) {
	d := newFakeDocker()
	ctx := context.Background()
	// First cluster: clusterID="old-cluster", CIDR=10.244.0.0/24.
	if _, err := bridge.Ensure(ctx, d, "sample", "frontend", "10.244.0.0/24", "old-cluster", nil); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	// Re-form in place: NEW clusterID, SAME (deployment, network), even
	// SAME CIDR — bridge must still be reclaimed (otherwise NetworkCreate
	// would later fail with "already exists").
	if _, err := bridge.Ensure(ctx, d, "sample", "frontend", "10.244.0.0/24", "new-cluster", nil); err != nil {
		t.Fatalf("second Ensure (mismatched cluster_id): %v", err)
	}
	if len(d.networks) != 1 {
		t.Fatalf("expected exactly 1 network after recreate, got %d", len(d.networks))
	}
	for _, n := range d.networks {
		if got := n.Labels["jaco.cluster_id"]; got != "new-cluster" {
			t.Errorf("recreated network cluster_id label = %q, want new-cluster", got)
		}
	}
}

// TestEnsure_RefusesForeignDockerNetwork: if a docker network with our name
// exists but lacks any jaco.* labels (operator-created collision), bail
// rather than tear down foreign state.
func TestEnsure_RefusesForeignDockerNetwork(t *testing.T) {
	d := newFakeDocker()
	ctx := context.Background()
	// Plant a non-JACO network with the same name as we'd create.
	if _, err := d.NetworkCreate(ctx, "jaco_sample_frontend", dnet.CreateOptions{
		Driver: "bridge",
		IPAM:   &dnet.IPAM{Driver: "default", Config: []dnet.IPAMConfig{{Subnet: "10.244.0.0/24", Gateway: "10.244.0.1"}}},
		Labels: map[string]string{"owner": "someone-else"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Ensure(ctx, d, "sample", "frontend", "10.244.0.0/24", "cluster-x", nil); err == nil {
		t.Errorf("Ensure should refuse a name-collision against a non-JACO network")
	}
}

func TestEnsure_RejectsEmptyArgs(t *testing.T) {
	d := newFakeDocker()
	if _, err := bridge.Ensure(context.Background(), d, "", "frontend", "10.244.5.0/24", "cluster-x", nil); err == nil {
		t.Errorf("empty deployment accepted")
	}
	if _, err := bridge.Ensure(context.Background(), d, "sample", "", "10.244.5.0/24", "cluster-x", nil); err == nil {
		t.Errorf("empty network accepted")
	}
	if _, err := bridge.Ensure(context.Background(), d, "sample", "frontend", "", "cluster-x", nil); err == nil {
		t.Errorf("empty cidr accepted")
	}
	if _, err := bridge.Ensure(context.Background(), d, "sample", "frontend", "10.244.5.0/24", "", nil); err == nil {
		t.Errorf("empty clusterID accepted")
	}
}

func TestTeardown_RemovesNetwork(t *testing.T) {
	d := newFakeDocker()
	if _, err := bridge.Ensure(context.Background(), d, "sample", "frontend", "10.244.5.0/24", "cluster-x", nil); err != nil {
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
