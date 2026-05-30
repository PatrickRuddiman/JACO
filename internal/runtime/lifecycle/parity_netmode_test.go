package lifecycle

import (
	"errors"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestBuildConfig_NetworkModeNone — issue #121: `network_mode: none`
// sets HostConfig.NetworkMode to "none" verbatim, leaves the
// NetworkingConfig EndpointsConfig empty so the runtime's NetworkConnect
// loop in lifecycle.Start is a no-op, and never consults the resolver
// (passing nil here is intentional — proves the resolver isn't needed
// for `none`).
func TestBuildConfig_NetworkModeNone(t *testing.T) {
	spec := compose.ContainerSpec{
		Image:       "alpine:3.20",
		Service:     "oneshot",
		Deployment:  "app",
		Networks:    []string{"jaco_app__default"}, // should be ignored
		NetworkMode: "none",
	}
	_, hc, netCfg, err := buildConfig(spec, nil)
	if err != nil {
		t.Fatalf("buildConfig: unexpected err: %v", err)
	}
	if string(hc.NetworkMode) != "none" {
		t.Errorf("HostConfig.NetworkMode = %q, want \"none\"", hc.NetworkMode)
	}
	if len(netCfg.EndpointsConfig) != 0 {
		t.Errorf("EndpointsConfig = %v, want empty (per-deployment bridge attach skipped for network_mode)", netCfg.EndpointsConfig)
	}
}

// TestBuildConfig_NetworkModeServiceResolves — issue #121: a
// `service:<name>` reference is resolved via the supplied resolver and
// the result lands on HostConfig.NetworkMode as `container:<id>`. The
// resolver receives the spec's deployment + the trimmed target name.
// EndpointsConfig stays empty (mutually exclusive with bridge attach).
func TestBuildConfig_NetworkModeServiceResolves(t *testing.T) {
	var seenDeployment, seenService string
	resolver := func(deployment, service string) (string, bool) {
		seenDeployment = deployment
		seenService = service
		return "c0ffee", true
	}
	spec := compose.ContainerSpec{
		Image:       "vector:0.39",
		Service:     "sidecar",
		Deployment:  "app",
		Networks:    []string{"jaco_app__default"}, // should be ignored
		NetworkMode: "service:app",
	}
	_, hc, netCfg, err := buildConfig(spec, resolver)
	if err != nil {
		t.Fatalf("buildConfig: unexpected err: %v", err)
	}
	if seenDeployment != "app" {
		t.Errorf("resolver received deployment = %q, want app", seenDeployment)
	}
	if seenService != "app" {
		t.Errorf("resolver received service = %q, want app", seenService)
	}
	if string(hc.NetworkMode) != "container:c0ffee" {
		t.Errorf("HostConfig.NetworkMode = %q, want container:c0ffee", hc.NetworkMode)
	}
	if len(netCfg.EndpointsConfig) != 0 {
		t.Errorf("EndpointsConfig = %v, want empty for network_mode=service:<name>", netCfg.EndpointsConfig)
	}
}

// TestBuildConfig_NetworkModeServiceNotReady — issue #121: when the
// resolver reports the target service has no running replica yet,
// buildConfig returns ErrNetworkModeTargetNotReady wrapped so the
// reconciler's existing retry loop picks it up and tries again on the
// next tick. Sidecars naturally bounce a few times waiting for their
// primary on the first deploy; steady-state cost is zero. Empty
// container id (ok=true, "") MUST behave the same as ok=false — a
// defensive fallback so a resolver bug can't accidentally produce
// `container:` (an invalid docker reference).
func TestBuildConfig_NetworkModeServiceNotReady(t *testing.T) {
	cases := []struct {
		name string
		fn   NetworkModeResolver
	}{
		{
			name: "resolver_returns_false",
			fn:   func(string, string) (string, bool) { return "", false },
		},
		{
			name: "resolver_returns_empty_id",
			fn:   func(string, string) (string, bool) { return "", true },
		},
		{
			name: "nil_resolver",
			fn:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := compose.ContainerSpec{
				Image:       "vector:0.39",
				Service:     "sidecar",
				Deployment:  "app",
				NetworkMode: "service:app",
			}
			_, _, _, err := buildConfig(spec, tc.fn)
			if err == nil {
				t.Fatalf("buildConfig: expected ErrNetworkModeTargetNotReady, got nil")
			}
			if !errors.Is(err, ErrNetworkModeTargetNotReady) {
				t.Errorf("err = %v, want errors.Is(ErrNetworkModeTargetNotReady)", err)
			}
		})
	}
}

// TestBuildConfig_NetworkModeMutuallyExclusiveWithBridges — issue #121:
// whenever NetworkMode is set (any non-empty value), the per-deployment
// network attach list (NetworkingConfig.EndpointsConfig) must be empty.
// Docker rejects setting both at the container level; this is the
// in-process guarantee.
func TestBuildConfig_NetworkModeMutuallyExclusiveWithBridges(t *testing.T) {
	cases := []struct {
		name    string
		spec    compose.ContainerSpec
		resolve NetworkModeResolver
	}{
		{
			name: "none_with_networks",
			spec: compose.ContainerSpec{
				Image: "alpine", Service: "s", Deployment: "d",
				Networks:    []string{"jaco_d__default", "jaco_d_frontend"},
				NetworkMode: "none",
			},
		},
		{
			name: "service_with_networks",
			spec: compose.ContainerSpec{
				Image: "alpine", Service: "s", Deployment: "d",
				Networks:    []string{"jaco_d__default", "jaco_d_frontend"},
				NetworkMode: "service:app",
			},
			resolve: func(string, string) (string, bool) { return "abc123", true },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, netCfg, err := buildConfig(tc.spec, tc.resolve)
			if err != nil {
				t.Fatalf("buildConfig: unexpected err: %v", err)
			}
			if len(netCfg.EndpointsConfig) != 0 {
				t.Errorf("EndpointsConfig = %v, want empty when NetworkMode is set", netCfg.EndpointsConfig)
			}
		})
	}
}

// TestBuildConfig_NetworkModeEmptyPreservesBridgeAttach — sanity: when
// NetworkMode is empty the existing per-deployment bridge attach path
// MUST run (bug 010), unchanged by #121.
func TestBuildConfig_NetworkModeEmptyPreservesBridgeAttach(t *testing.T) {
	spec := compose.ContainerSpec{
		Image: "nginx:1.27", Service: "web", Deployment: "app",
		Networks: []string{"jaco_app_frontend"},
	}
	_, hc, netCfg, err := buildConfig(spec, nil)
	if err != nil {
		t.Fatalf("buildConfig: unexpected err: %v", err)
	}
	if string(hc.NetworkMode) != "jaco_app_frontend" {
		t.Errorf("HostConfig.NetworkMode = %q, want jaco_app_frontend (bug 010 path)", hc.NetworkMode)
	}
	if _, ok := netCfg.EndpointsConfig["jaco_app_frontend"]; !ok {
		t.Errorf("EndpointsConfig missing jaco_app_frontend; got %v", netCfg.EndpointsConfig)
	}
}

// TestBuildConfig_NetworkModeStripsDockerConflicts — issue #121 found
// at integ smoke-test on the 3-node bed: when network_mode is set,
// docker rejects ContainerCreate with "conflicting options: dns and
// the network mode" if any of dns / dns_search / dns_opt / hostname /
// domainname / mac_address / extra_hosts are populated. The joined
// netns owns those (or in the case of `none`, nobody does), so
// copying defaults from a bridge we aren't attached to would be a bug
// even without docker's veto. buildConfig MUST zero these for both
// `none` and `service:<name>` paths.
func TestBuildConfig_NetworkModeStripsDockerConflicts(t *testing.T) {
	base := compose.ContainerSpec{
		Image:      "alpine:3.20",
		Service:    "sidecar",
		Deployment: "app",
		// Populate every field docker rejects in combination with
		// network_mode. If buildConfig leaves any one of them set,
		// the bed-observed ContainerCreate failure returns here.
		Hostname:    "sidecar.app",
		Domainname:  "jaco.internal",
		DNS:         []string{"1.1.1.1"},
		DNSSearch:   []string{"jaco.internal"},
		DNSOptions:  []string{"ndots:1"},
		ExtraHosts:  []string{"db:127.0.0.1"},
		DNSServers:  []string{"10.42.0.1"}, // runtime fallback also must NOT leak through
	}
	resolver := func(string, string) (string, bool) { return "c0ffee", true }

	cases := []struct {
		name        string
		networkMode string
		resolver    NetworkModeResolver
	}{
		{"none", "none", nil},
		{"service", "service:app", resolver},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			spec.NetworkMode = tc.networkMode
			cfg, hc, _, err := buildConfig(spec, tc.resolver)
			if err != nil {
				t.Fatalf("buildConfig: unexpected err: %v", err)
			}
			if cfg.Hostname != "" {
				t.Errorf("cfg.Hostname = %q, want empty (conflicts with network_mode)", cfg.Hostname)
			}
			if cfg.Domainname != "" {
				t.Errorf("cfg.Domainname = %q, want empty", cfg.Domainname)
			}
			if cfg.MacAddress != "" {
				t.Errorf("cfg.MacAddress = %q, want empty", cfg.MacAddress)
			}
			if hc.DNS != nil {
				t.Errorf("hostCfg.DNS = %v, want nil", hc.DNS)
			}
			if hc.DNSSearch != nil {
				t.Errorf("hostCfg.DNSSearch = %v, want nil", hc.DNSSearch)
			}
			if hc.DNSOptions != nil {
				t.Errorf("hostCfg.DNSOptions = %v, want nil", hc.DNSOptions)
			}
			if hc.ExtraHosts != nil {
				t.Errorf("hostCfg.ExtraHosts = %v, want nil", hc.ExtraHosts)
			}
		})
	}
}
