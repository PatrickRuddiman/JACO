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
	"net"
	"strings"

	"github.com/docker/docker/api/types/filters"
	dnet "github.com/docker/docker/api/types/network"

	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
)

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
// create or a reuse was performed.
func Ensure(ctx context.Context, d dockerx.Docker, deployment, network, cidr, clusterID string) (string, error) {
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

	// Idempotency: if a network with our name + cluster_id label already
	// exists, do nothing.
	args := filters.NewArgs()
	args.Add("label", "jaco.cluster_id="+clusterID)
	args.Add("label", "jaco.deployment="+deployment)
	args.Add("label", "jaco.network="+network)
	existing, err := d.NetworkList(ctx, dnet.ListOptions{Filters: args})
	if err != nil {
		return "", fmt.Errorf("NetworkList: %w", err)
	}
	for _, n := range existing {
		if n.Name == name {
			return name, nil
		}
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
		},
	}
	if _, err := d.NetworkCreate(ctx, name, opts); err != nil {
		return "", fmt.Errorf("NetworkCreate %s: %w", name, err)
	}
	return name, nil
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

// silence unused
var _ = strings.HasPrefix
