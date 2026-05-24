//go:build docker

package discovery_test

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dnet "github.com/docker/docker/api/types/network"
	hraft "github.com/hashicorp/raft"
	mdns "github.com/miekg/dns"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
	jdns "github.com/PatrickRuddiman/jaco/internal/discovery/dns"
	"github.com/PatrickRuddiman/jaco/internal/discovery/ipam"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
)

// TestDiscovery_PerHostSubnetBridgeResolution ties the issue #28 datapath
// together on a single host with a real Docker engine: a per-host /24 is
// allocated, the bridge is created at MTU 1420 with that subnet, a real
// container gets an IP inside it, and the responder resolves the service name
// (bare + FQDN) to that IP. Cross-host routing / AllowedIPs / SNAT are
// exercised by the privileged 3-node isolation rig, not here.
//
// Gated on JACO_INTEGRATION_DOCKER + the `docker` build tag.
func TestDiscovery_PerHostSubnetBridgeResolution(t *testing.T) {
	if os.Getenv("JACO_INTEGRATION_DOCKER") == "" {
		t.Skip("set JACO_INTEGRATION_DOCKER=1 to enable")
	}
	d, err := dockerx.New("")
	if err != nil {
		t.Skipf("docker unreachable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const dep, netName, host, clusterID = "itapp", "frontend", "host-a", "cluster-it"

	// 1. Allocate the per-host /24 through the real ipam + FSM (no raft net).
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var idx uint64
	apply := func(b []byte) error { idx++; f.Apply(&hraft.Log{Index: idx, Data: b}); return nil }
	allocator, err := ipam.New(st, apply, ipam.DefaultPoolCIDR)
	if err != nil {
		t.Fatalf("ipam.New: %v", err)
	}
	sn, err := allocator.Allocate(dep, netName, host)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	cidr := sn.GetCidr()
	if _, ok := st.Subnets.Get(state.SubnetKey(dep, netName, host)); !ok {
		t.Fatalf("subnet (%s,%s,%s) not in state", dep, netName, host)
	}

	// 2. Create the bridge and assert MTU 1420 + the allocated subnet.
	dockerNet := bridge.DockerNetworkName(dep, netName)
	t.Cleanup(func() { _ = bridge.Teardown(context.Background(), d, dep, netName) })
	if _, err := bridge.Ensure(ctx, d, dep, netName, cidr, clusterID); err != nil {
		t.Fatalf("bridge.Ensure: %v", err)
	}
	nets, err := d.NetworkList(ctx, dnet.ListOptions{Filters: filters.NewArgs(filters.Arg("name", dockerNet))})
	if err != nil {
		t.Fatalf("NetworkList: %v", err)
	}
	var found *dnet.Summary
	for i := range nets {
		if nets[i].Name == dockerNet {
			found = &nets[i]
		}
	}
	if found == nil {
		t.Fatalf("docker network %s not created", dockerNet)
	}
	if got := found.Options["com.docker.network.driver.mtu"]; got != "1420" {
		t.Errorf("bridge MTU = %q, want 1420", got)
	}

	// 3. Start a container on the bridge and read its IP.
	if rc, perr := d.ImagePull(ctx, "busybox", image.PullOptions{}); perr == nil {
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	cresp, err := d.ContainerCreate(ctx,
		&container.Config{Image: "busybox", Cmd: []string{"sleep", "60"}, Labels: map[string]string{"jaco.cluster_id": clusterID}},
		&container.HostConfig{NetworkMode: container.NetworkMode(dockerNet)},
		&dnet.NetworkingConfig{EndpointsConfig: map[string]*dnet.EndpointSettings{dockerNet: {}}},
		nil, "jaco-it-web")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() { _ = d.ContainerRemove(context.Background(), cresp.ID, container.RemoveOptions{Force: true}) })
	if err := d.ContainerStart(ctx, cresp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	info, err := d.ContainerInspect(ctx, cresp.ID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	ep := info.NetworkSettings.Networks[dockerNet]
	if ep == nil || ep.IPAddress == "" {
		t.Fatalf("container has no IP on %s", dockerNet)
	}
	ip := net.ParseIP(ep.IPAddress)
	if _, ipnet, _ := net.ParseCIDR(cidr); ipnet == nil || !ipnet.Contains(ip) {
		t.Fatalf("container IP %s not inside allocated subnet %s", ip, cidr)
	}

	// 4. The responder resolves the service name (bare + FQDN) to the real IP.
	resp := jdns.New(jdns.Scope{Deployment: dep, Network: netName}, jdns.ServiceMap{"web": {ip}}, nil)
	for _, name := range []string{"web", "web." + dep + ".jaco.internal"} {
		q := new(mdns.Msg)
		q.SetQuestion(mdns.Fqdn(name), mdns.TypeA)
		out := resp.Handle(q)
		if len(out.Answer) != 1 {
			t.Fatalf("%s: answers = %d, want 1", name, len(out.Answer))
		}
		a, ok := out.Answer[0].(*mdns.A)
		if !ok || !a.A.Equal(ip.To4()) {
			t.Errorf("%s: answer = %v, want %s", name, out.Answer[0], ip)
		}
	}
}
