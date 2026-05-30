package lifecycle

import (
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/go-units"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// buildConfig projects a compose.ContainerSpec into docker's three config
// structs. The first declared network attaches at create-time via
// NetworkingConfig.EndpointsConfig; additional networks attach via
// NetworkConnect after create. NetworkMode=none (the previous default)
// was rejected by Docker when followed by a NetworkConnect — see bug 010.
func buildConfig(spec compose.ContainerSpec) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	cfg := &container.Config{
		Image:       spec.Image,
		Cmd:         strslice.StrSlice(spec.Command),
		Entrypoint:  strslice.StrSlice(spec.Entrypoint),
		Env:         spec.Env,
		WorkingDir:  spec.WorkingDir,
		User:        spec.User,
		Labels:      spec.Labels,
		Hostname:    spec.Hostname,
		Domainname:  spec.Domainname,
		StopSignal:  spec.StopSignal,
		StopTimeout: spec.StopGracePeriodSeconds,
	}
	if hc := spec.Healthcheck; hc != nil {
		cfg.Healthcheck = &container.HealthConfig{
			Test:        hc.Test,
			Interval:    hc.Interval,
			Timeout:     hc.Timeout,
			Retries:     hc.Retries,
			StartPeriod: hc.StartPeriod,
		}
	}

	// DNS precedence (issue #117): an explicit compose `dns:` overrides
	// the runtime-resolved per-bridge DNSServers. Empty compose `dns:` →
	// keep the runtime resolvers so JACO's per-bridge DNS Manager wins
	// (same behavior as before #117).
	dnsServers := spec.DNS
	if len(dnsServers) == 0 {
		dnsServers = spec.DNSServers
	}
	hostCfg := &container.HostConfig{
		ReadonlyRootfs: spec.ReadOnly,
		CapAdd:         spec.CapAdd,
		CapDrop:        spec.CapDrop,
		Sysctls:        spec.Sysctls,
		Mounts:         toDockerMounts(spec.Mounts),
		Tmpfs:          toTmpfsMap(spec.Tmpfs),
		DNS:            dnsServers,
		DNSSearch:      spec.DNSSearch,
		DNSOptions:     spec.DNSOptions,
		ExtraHosts:     spec.ExtraHosts,
		Init:           spec.Init,
		ShmSize:        spec.ShmSizeBytes,
		IpcMode:        container.IpcMode(spec.IpcMode),
		PidMode:        container.PidMode(spec.PidMode),
		UTSMode:        container.UTSMode(spec.UTSMode),
		UsernsMode:     container.UsernsMode(spec.UsernsMode),
		CgroupnsMode:   container.CgroupnsMode(spec.CgroupnsMode),

		// Privileged + security_opt (#119). Validator + Apply admission
		// guard these so reaching this line means the operator already
		// opted in on both ends — forward verbatim to docker.
		Privileged: spec.Privileged,
		SecurityOpt: spec.SecurityOpt,

		LogConfig:      toDockerLogConfig(spec.LogConfig),
		Resources: container.Resources{
			Ulimits: toUlimitsList(spec.Ulimits),
			// Per-replica CPU/memory cgroup limits (issue #49). The compose
			// loader already resolved these from either deploy.resources or
			// the legacy top-level keys; zero values are docker's "unset".
			NanoCPUs:          spec.NanoCPUs,
			Memory:            spec.MemoryBytes,
			MemoryReservation: spec.MemoryReservationBytes,
			CPUShares:         spec.CPUShares,
			CpusetCpus:        spec.CpusetCpus,
			PidsLimit:         spec.PidsLimit,
			CgroupParent:      spec.CgroupParent,
			// Host devices (#115) and modern GPU requests (#116). Both
			// live on container.Resources; nil/empty preserves docker
			// default ("no override").
			Devices:        toDockerDevices(spec.Devices),
			DeviceRequests: toDockerDeviceRequests(spec.GPURequests),
		},
	}

	netCfg := &network.NetworkingConfig{}
	if len(spec.Networks) > 0 {
		// Bug 010: attach the first network at create-time. Subsequent
		// networks attach via NetworkConnect in lifecycle.Start.
		// HostConfig.NetworkMode is implicitly the first network's name
		// when EndpointsConfig has exactly one entry.
		hostCfg.NetworkMode = container.NetworkMode(spec.Networks[0])
		netCfg.EndpointsConfig = map[string]*network.EndpointSettings{
			spec.Networks[0]: {Aliases: serviceAliases(spec)},
		}
	}
	return cfg, hostCfg, netCfg
}

// serviceAliases are the names Docker's embedded DNS (127.0.0.11) resolves to
// this container for same-host service discovery (issue #28) — a
// belt-and-suspenders alongside the per-bridge JACO responder. Empty
// deployment/service still yields the bare-service alias.
func serviceAliases(spec compose.ContainerSpec) []string {
	fqdn := spec.Service + "." + spec.Deployment
	return []string{spec.Service, fqdn, fqdn + ".jaco.internal"}
}

func toDockerMounts(in []compose.Mount) []mount.Mount {
	if len(in) == 0 {
		return nil
	}
	out := make([]mount.Mount, 0, len(in))
	for _, m := range in {
		dm := mount.Mount{
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		}
		switch m.Type {
		case "volume":
			dm.Type = mount.TypeVolume
		case "bind", "":
			dm.Type = mount.TypeBind
		default:
			dm.Type = mount.Type(m.Type)
		}
		out = append(out, dm)
	}
	return out
}

func toTmpfsMap(in []string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for _, t := range in {
		out[t] = ""
	}
	return out
}

// toDockerLogConfig projects the spec's resolved log configuration into
// docker's container.LogConfig (whose driver field is named Type). A nil spec
// LogConfig yields the zero value, which docker treats as "use the daemon's
// default log driver" — exactly the behavior we want when compose declared
// nothing (issue #94).
func toDockerLogConfig(in *compose.LogConfig) container.LogConfig {
	if in == nil {
		return container.LogConfig{}
	}
	return container.LogConfig{
		Type:   in.Driver,
		Config: in.Options,
	}
}

func toUlimitsList(in map[string]compose.Ulimit) []*units.Ulimit {
	if len(in) == 0 {
		return nil
	}
	out := make([]*units.Ulimit, 0, len(in))
	for name, u := range in {
		out = append(out, &units.Ulimit{Name: name, Soft: u.Soft, Hard: u.Hard})
	}
	return out
}

// toDockerDevices projects the spec's host-device bindings into docker's
// container.DeviceMapping (issue #115). Nil/empty input → nil so the
// HostConfig field stays unset (docker default: no devices forwarded).
func toDockerDevices(in []compose.DeviceMapping) []container.DeviceMapping {
	if len(in) == 0 {
		return nil
	}
	out := make([]container.DeviceMapping, len(in))
	for i, d := range in {
		out[i] = container.DeviceMapping{
			PathOnHost:        d.Source,
			PathInContainer:   d.Target,
			CgroupPermissions: d.Permissions,
		}
	}
	return out
}

// toDockerDeviceRequests projects the spec's GPU requests into docker's
// container.DeviceRequest (issue #116). The spec's flat AND-list of
// capabilities wraps into docker's [][]string OR-of-AND form (one OR
// element containing the full AND list — matches what compose-spec
// renders for `gpus: all` → `[["gpu"]]`). Nil/empty input → nil so the
// HostConfig stays free of an empty request.
func toDockerDeviceRequests(in []compose.GPURequest) []container.DeviceRequest {
	if len(in) == 0 {
		return nil
	}
	out := make([]container.DeviceRequest, len(in))
	for i, r := range in {
		var caps [][]string
		if len(r.Capabilities) > 0 {
			caps = [][]string{append([]string(nil), r.Capabilities...)}
		}
		out[i] = container.DeviceRequest{
			Driver:       r.Driver,
			Count:        int(r.Count),
			DeviceIDs:    append([]string(nil), r.DeviceIDs...),
			Capabilities: caps,
			Options:      cloneStringMap(r.Options),
		}
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
