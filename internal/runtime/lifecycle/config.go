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
// structs. NetworkMode is forced to "none" — the runtime caller attaches the
// container to JACO bridges via NetworkConnect after create (task 27).
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
		NetworkMode:    container.NetworkMode("none"),
		CapAdd:         spec.CapAdd,
		CapDrop:        spec.CapDrop,
		Sysctls:        spec.Sysctls,
		Mounts:         toDockerMounts(spec.Mounts),
		Tmpfs:          toTmpfsMap(spec.Tmpfs),
		Resources: container.Resources{
			Ulimits: toUlimitsList(spec.Ulimits),
		},
	}

	netCfg := &network.NetworkingConfig{}
	return cfg, hostCfg, netCfg
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
