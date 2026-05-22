package compose

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

// ToContainerSpec projects a compose ServiceConfig into a per-replica
// ContainerSpec. opts supplies the JACO metadata (cluster id, deployment,
// replica id/index, raft index) that gets stamped onto Labels and ID fields.
func ToContainerSpec(svc types.ServiceConfig, opts SpecOptions) ContainerSpec {
	spec := ContainerSpec{
		ClusterID:    opts.ClusterID,
		Deployment:   opts.Deployment,
		Service:      opts.Service,
		ReplicaID:    opts.ReplicaID,
		ReplicaIndex: opts.ReplicaIndex,
		RaftIndex:    opts.RaftIndex,

		Image:      svc.Image,
		Command:    cloneStringList(svc.Command),
		Entrypoint: cloneStringList(svc.Entrypoint),
		WorkingDir: svc.WorkingDir,
		User:       svc.User,
		ReadOnly:   svc.ReadOnly,
	}

	spec.Env = envFromCompose(svc.Environment)
	spec.Labels = labelsWithJACO(svc.Labels, opts)
	spec.Mounts = mountsFromCompose(svc.Volumes)
	spec.Tmpfs = cloneStringList(svc.Tmpfs)
	spec.CapAdd = cloneStringList(svc.CapAdd)
	spec.CapDrop = cloneStringList(svc.CapDrop)
	spec.Sysctls = mapStringToMap(svc.Sysctls)
	spec.Ulimits = ulimitsFromCompose(svc.Ulimits)
	spec.Healthcheck = healthcheckFromCompose(svc.HealthCheck)
	spec.Networks = networksFromCompose(svc.Networks, opts.Deployment)
	spec.Ports = portsFromCompose(svc.Ports)
	return spec
}

// ContainerName is the conventional docker container name JACO assigns to
// each replica — uses the replica id so containers are unambiguous in
// `docker ps`.
func ContainerName(opts SpecOptions) string {
	return "jaco_" + opts.ReplicaID
}

// envFromCompose flattens MappingWithEquals into sorted KEY=value strings so
// the resulting spec is deterministic across reconciles.
func envFromCompose(env types.MappingWithEquals) []string {
	out := make([]string, 0, len(env))
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := env[k]; v != nil {
			out = append(out, k+"="+*v)
		} else {
			out = append(out, k+"=")
		}
	}
	return out
}

// labelsWithJACO merges the user-supplied compose labels with the six JACO
// labels. JACO labels always win on conflict — they're the source of truth
// for orphan reconciliation.
func labelsWithJACO(user types.Labels, opts SpecOptions) map[string]string {
	out := make(map[string]string, len(user)+6)
	for k, v := range user {
		out[k] = v
	}
	out["jaco.cluster_id"] = opts.ClusterID
	out["jaco.deployment"] = opts.Deployment
	out["jaco.service"] = opts.Service
	out["jaco.replica_id"] = opts.ReplicaID
	out["jaco.replica_index"] = strconv.Itoa(opts.ReplicaIndex)
	out["jaco.raft_index"] = strconv.FormatUint(opts.RaftIndex, 10)
	return out
}

func mountsFromCompose(vols []types.ServiceVolumeConfig) []Mount {
	if len(vols) == 0 {
		return nil
	}
	out := make([]Mount, 0, len(vols))
	for _, v := range vols {
		out = append(out, Mount{
			Type:     v.Type,
			Source:   v.Source,
			Target:   v.Target,
			ReadOnly: v.ReadOnly,
		})
	}
	return out
}

func mapStringToMap(m types.Mapping) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func ulimitsFromCompose(in map[string]*types.UlimitsConfig) map[string]Ulimit {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]Ulimit, len(in))
	for k, v := range in {
		if v == nil {
			continue
		}
		out[k] = Ulimit{Soft: int64(v.Soft), Hard: int64(v.Hard)}
	}
	return out
}

func healthcheckFromCompose(hc *types.HealthCheckConfig) *Healthcheck {
	if hc == nil {
		return nil
	}
	out := &Healthcheck{Test: cloneStringList([]string(hc.Test))}
	if hc.Interval != nil {
		out.Interval = time.Duration(*hc.Interval)
	}
	if hc.Timeout != nil {
		out.Timeout = time.Duration(*hc.Timeout)
	}
	if hc.Retries != nil {
		out.Retries = int(*hc.Retries)
	}
	if hc.StartPeriod != nil {
		out.StartPeriod = time.Duration(*hc.StartPeriod)
	}
	return out
}

// networksFromCompose translates compose's service-level `networks:` map
// into the docker network names JACO's discovery slice creates. Empty
// declaration → ["jaco_<deployment>__default"]. Order matches the slice's
// "compose-declared attach order" rule (discovery §3 decision 9).
func networksFromCompose(in map[string]*types.ServiceNetworkConfig, deployment string) []string {
	if len(in) == 0 {
		return []string{networkName(deployment, "_default")}
	}
	// compose-go normalizes service `networks: [a, b]` into the map form;
	// preserve declaration order via the per-entry Priority field when set,
	// otherwise sort alphabetically for determinism.
	type ent struct {
		name     string
		priority int
		hasPrio  bool
	}
	entries := make([]ent, 0, len(in))
	for name, cfg := range in {
		e := ent{name: name}
		if cfg != nil {
			e.priority = cfg.Priority
			e.hasPrio = cfg.Priority != 0
		}
		entries = append(entries, e)
	}
	// Sort by priority desc (higher first), then by name asc for stability.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].hasPrio || entries[j].hasPrio {
			return entries[i].priority > entries[j].priority
		}
		return entries[i].name < entries[j].name
	})
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, networkName(deployment, e.name))
	}
	return out
}

func networkName(deployment, network string) string {
	// compose-go normalizes services with no explicit networks: into the
	// implicit "default" network. JACO's convention (see discovery slice §3)
	// names the implicit default "_default" so it can't collide with a
	// user-declared network. Translate at the boundary.
	if network == "default" {
		network = "_default"
	}
	return "jaco_" + deployment + "_" + network
}

func portsFromCompose(ports []types.ServicePortConfig) []PortDecl {
	if len(ports) == 0 {
		return nil
	}
	out := make([]PortDecl, 0, len(ports))
	for _, p := range ports {
		decl := PortDecl{
			Container: int(p.Target),
			Protocol:  p.Protocol,
		}
		if p.Published != "" {
			if v, err := strconv.Atoi(p.Published); err == nil {
				decl.Host = v
			}
		}
		if decl.Protocol == "" {
			decl.Protocol = "tcp"
		}
		out = append(out, decl)
	}
	return out
}

func cloneStringList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// silence unused-symbol checks until later tasks consume the helpers.
var _ = fmt.Sprintf
