// Package bridge owns the per-(deployment, network) docker bridge: name
// derivation, idempotent Ensure, and Teardown. Bridge naming honors two
// constraints: docker network names are deployment/network-readable
// (`jaco_<dep>_<net>`); Linux bridge interface names sit inside the 15-char
// kernel limit (`jaco-<dep4>-<net4>` where dep4/net4 are the first 4 hex
// chars of sha1(name)).
package bridge

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dnet "github.com/docker/docker/api/types/network"

	"github.com/PatrickRuddiman/jaco/internal/logging"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
)

// BridgeMTU is the MTU set on every JACO docker bridge. Capped below the
// default 1500 to leave room for WireGuard encapsulation overhead so
// cross-host packets routed over jaco0 don't fragment (issue #28).
const BridgeMTU = 1420

// DockerNetworkName returns the user-facing docker network identifier used
// by NetworkConnect calls. compose's "default" network is renamed to
// "_default" at the network-name boundary (matches compose.networkName from
// task 13).
func DockerNetworkName(deployment, network string) string {
	if network == "default" {
		network = "_default"
	}
	return "jaco_" + deployment + "_" + network
}

// NetworkNameFromDockerName is the inverse of DockerNetworkName — pulls
// the network suffix out of `jaco_<deployment>_<network>`. Returns ""
// when the input doesn't match the JACO pattern.
func NetworkNameFromDockerName(s string) string {
	const prefix = "jaco_"
	if !strings.HasPrefix(s, prefix) {
		return ""
	}
	tail := s[len(prefix):]
	idx := strings.IndexByte(tail, '_')
	if idx < 0 {
		return ""
	}
	return tail[idx+1:]
}

// LinuxBridgeName returns the Linux bridge interface name for the
// (deployment, network) pair. Fits the 15-char limit via short sha1 hashes.
func LinuxBridgeName(deployment, network string) string {
	if network == "default" {
		network = "_default"
	}
	return "jaco-" + shortHash(deployment) + "-" + shortHash(network)
}

func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:4]
}

// GatewayIP returns the first usable address of the given /24 subnet — the
// docker bridge's address inside the network, also served as the
// containers' resolv.conf nameserver.
func GatewayIP(cidr string) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("GatewayIP: parse %q: %w", cidr, err)
	}
	ip := ipnet.IP.To4()
	if ip == nil {
		return "", fmt.Errorf("GatewayIP: %q is not IPv4", cidr)
	}
	gw := net.IPv4(ip[0], ip[1], ip[2], 1)
	return gw.String(), nil
}

// Ensure idempotently creates the docker network backing (deployment,
// network). Returns the docker network name regardless of whether a fresh
// create or a reuse was performed. logger may be nil (→ discard); the daemon
// passes its runtime/reconciler-scoped logger so bridge drift/recreate
// decisions appear in the journal.
func Ensure(ctx context.Context, d dockerx.Docker, deployment, network, cidr, clusterID string, logger *slog.Logger) (string, error) {
	if logger == nil {
		logger = logging.Discard()
	}
	if deployment == "" || network == "" {
		return "", fmt.Errorf("Ensure: deployment + network are required")
	}
	if cidr == "" {
		return "", fmt.Errorf("Ensure: cidr is required")
	}
	if clusterID == "" {
		return "", fmt.Errorf("Ensure: clusterID is required")
	}
	name := DockerNetworkName(deployment, network)

	// Look up any existing docker network with our name. Issue #42: filter
	// by NAME, not cluster_id — when raft state is wiped and the cluster is
	// re-formed in place, the stale bridge's jaco.cluster_id label points at
	// the OLD cluster, so a cluster_id-scoped filter would miss it and the
	// follow-up NetworkCreate would fail with "network already exists".
	// A name match plus a jaco.* label (i.e. JACO-owned by convention) is
	// the strongest signal we have.
	args := filters.NewArgs()
	args.Add("name", name)
	existing, err := d.NetworkList(ctx, dnet.ListOptions{Filters: args})
	if err != nil {
		return "", fmt.Errorf("NetworkList: %w", err)
	}
	for _, n := range existing {
		if n.Name != name {
			continue
		}
		if !isJACOBridge(n.Labels) {
			// Same name, but no jaco.* labels — operator-created. Bail
			// rather than blow away foreign state.
			return "", fmt.Errorf("Ensure: docker network %s exists but has no jaco.* labels — refusing to reconcile", name)
		}
		// Compare the LIVE IPAM subnet against the desired CIDR. Labels
		// (jaco.subnet) reflect the creation-time CIDR and can diverge in
		// edge cases — trust NetworkInspect.
		insp, err := d.NetworkInspect(ctx, n.ID, dnet.InspectOptions{})
		if err != nil {
			return "", fmt.Errorf("NetworkInspect %s: %w", n.ID, err)
		}
		if liveSubnet(insp) == cidr && insp.Labels["jaco.cluster_id"] == clusterID {
			return name, nil
		}
		// Stop attached containers before recreating — they're about to be
		// re-scheduled with the new bridge anyway.
		logger.Info("bridge drift detected, recreating",
			"network", name, "live_subnet", liveSubnet(insp), "desired_subnet", cidr,
			"live_cluster_id", insp.Labels["jaco.cluster_id"], "desired_cluster_id", clusterID)
		for cid, ep := range insp.Containers {
			logger.Info("stop+remove attached container before recreate",
				"network", name, "container", ep.Name, "container_id", cid)
			timeout := 10
			if err := d.ContainerStop(ctx, cid, container.StopOptions{Timeout: &timeout}); err != nil {
				// Best-effort; continue to ContainerRemove which forces.
				logger.Warn("ContainerStop failed, continuing", "network", name, "container_id", cid, "error", err)
			}
			if err := d.ContainerRemove(ctx, cid, container.RemoveOptions{Force: true}); err != nil {
				return "", fmt.Errorf("ContainerRemove %s (attached to %s): %w", cid, name, err)
			}
		}
		if err := d.NetworkRemove(ctx, n.ID); err != nil {
			return "", fmt.Errorf("NetworkRemove %s (stale subnet): %w", n.ID, err)
		}
		break
	}

	gw, err := GatewayIP(cidr)
	if err != nil {
		return "", err
	}
	opts := dnet.CreateOptions{
		Driver: "bridge",
		IPAM: &dnet.IPAM{
			Driver: "default",
			Config: []dnet.IPAMConfig{{Subnet: cidr, Gateway: gw}},
		},
		Labels: map[string]string{
			"jaco.cluster_id": clusterID,
			"jaco.deployment": deployment,
			"jaco.network":    network,
			"jaco.subnet":     cidr,
		},
		Options: map[string]string{
			"com.docker.network.bridge.name": LinuxBridgeName(deployment, network),
			// Cap MTU below the WireGuard overhead so cross-host packets
			// (issue #28) don't fragment when routed over jaco0.
			"com.docker.network.driver.mtu": strconv.Itoa(BridgeMTU),
		},
	}
	if _, err := d.NetworkCreate(ctx, name, opts); err != nil {
		return "", fmt.Errorf("NetworkCreate %s: %w", name, err)
	}
	return name, nil
}

// liveSubnet returns the first IPAM subnet on the inspected network, or "" when
// none is configured. The JACO bridge create path sets exactly one Config
// entry, so the first slot is the canonical live CIDR.
func liveSubnet(insp dnet.Inspect) string {
	if len(insp.IPAM.Config) == 0 {
		return ""
	}
	return insp.IPAM.Config[0].Subnet
}

// isJACOBridge reports whether labels look like a JACO-owned bridge. We only
// require jaco.deployment + jaco.network: jaco.cluster_id can validly differ
// (e.g. after raft wipe + re-init the docker network still carries the OLD
// cluster_id label until Ensure recreates it).
func isJACOBridge(labels map[string]string) bool {
	return labels["jaco.deployment"] != "" && labels["jaco.network"] != ""
}

// Teardown removes the docker network for (deployment, network). No-op when
// the network doesn't exist.
func Teardown(ctx context.Context, d dockerx.Docker, deployment, network string) error {
	name := DockerNetworkName(deployment, network)
	args := filters.NewArgs()
	args.Add("name", name)
	existing, err := d.NetworkList(ctx, dnet.ListOptions{Filters: args})
	if err != nil {
		return fmt.Errorf("NetworkList: %w", err)
	}
	for _, n := range existing {
		if n.Name == name {
			if err := d.NetworkRemove(ctx, n.ID); err != nil {
				return fmt.Errorf("NetworkRemove %s: %w", n.ID, err)
			}
		}
	}
	return nil
}
