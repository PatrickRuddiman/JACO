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
		Image:      spec.Image,
		Cmd:        strslice.StrSlice(spec.Command),
		Entrypoint: strslice.StrSlice(spec.Entrypoint),
		Env:        spec.Env,
		WorkingDir: spec.WorkingDir,
		User:       spec.User,
		Labels:     spec.Labels,
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

	hostCfg := &container.HostConfig{
		ReadonlyRootfs: spec.ReadOnly,
		CapAdd:         spec.CapAdd,
		CapDrop:        spec.CapDrop,
		Sysctls:        spec.Sysctls,
		Mounts:         toDockerMounts(spec.Mounts),
		Tmpfs:          toTmpfsMap(spec.Tmpfs),
		DNS:            spec.DNSServers,
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
