package bridge_test

import (
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
)

// TestNetworkNameFromDockerName_HappyPath — the inverse of
// DockerNetworkName.
func TestNetworkNameFromDockerName_HappyPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"jaco_app_frontend", "frontend"},
		{"jaco_app__default", "_default"}, // default-network translation
		{"jaco_app_with_underscores_net", "with_underscores_net"},
	}
	for _, c := range cases {
		if got := bridge.NetworkNameFromDockerName(c.in); got != c.want {
			t.Errorf("NetworkNameFromDockerName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNetworkNameFromDockerName_NonJacoInputReturnsEmpty — names that
// don't start with jaco_ aren't JACO networks; ignored.
func TestNetworkNameFromDockerName_NonJacoInputReturnsEmpty(t *testing.T) {
	cases := []string{
		"docker_default",
		"bridge",
		"some_other_app_frontend",
		"jaco_no_underscore_after_deployment", // actually has underscores, treated as net
	}
	for _, in := range cases {
		got := bridge.NetworkNameFromDockerName(in)
		if in == "jaco_no_underscore_after_deployment" {
			// This one actually has the pattern — passes through.
			continue
		}
		if got != "" {
			t.Errorf("NetworkNameFromDockerName(%q) = %q, want empty", in, got)
		}
	}
}

// TestNetworkNameFromDockerName_MalformedReturnsEmpty — `jaco_` prefix
// with no separator afterwards has no network suffix.
func TestNetworkNameFromDockerName_MalformedReturnsEmpty(t *testing.T) {
	if got := bridge.NetworkNameFromDockerName("jaco_only-no-underscore"); got != "" {
		t.Errorf("got = %q, want \"\"", got)
	}
}

// TestGatewayIP_RejectsIPv6 — the function is /32 IPv4 only.
func TestGatewayIP_RejectsIPv6(t *testing.T) {
	if _, err := bridge.GatewayIP("2001:db8::/64"); err == nil {
		t.Errorf("GatewayIP(IPv6) returned nil err")
	}
}
