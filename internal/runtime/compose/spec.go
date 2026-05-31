package compose

import (
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
	spec.LogConfig = logConfigFromCompose(svc.Logging)

	res := resolveResources(svc)
	spec.NanoCPUs = res.NanoCPUs
	spec.MemoryBytes = res.MemoryBytes
	spec.MemoryReservationBytes = res.MemoryReservationBytes
	spec.CPUShares = res.CPUShares
	spec.CpusetCpus = res.CpusetCpus
	spec.PidsLimit = res.PidsLimit

	// Shutdown semantics (#114)
	spec.StopSignal = svc.StopSignal
	if svc.StopGracePeriod != nil {
		secs := int(time.Duration(*svc.StopGracePeriod).Seconds())
		spec.StopGracePeriodSeconds = &secs
	}

	// Trivial host/dns/init/shm passthroughs (#117)
	spec.Hostname = svc.Hostname
	spec.Domainname = svc.DomainName
	spec.ExtraHosts = svc.ExtraHosts.AsList(":")
	spec.DNS = cloneStringList(svc.DNS)
	spec.DNSSearch = cloneStringList(svc.DNSSearch)
	spec.DNSOptions = cloneStringList(svc.DNSOpts)
	if svc.Init != nil {
		v := *svc.Init
		spec.Init = &v
	}
	spec.ShmSizeBytes = int64(svc.ShmSize)

	// Namespace knobs (#118)
	spec.IpcMode = svc.Ipc
	spec.PidMode = svc.Pid
	spec.UTSMode = svc.Uts
	spec.UsernsMode = svc.UserNSMode
	spec.CgroupnsMode = svc.Cgroup
	spec.CgroupParent = svc.CgroupParent

	// Host devices (#115)
	spec.Devices = devicesFromCompose(svc.Devices)

	// GPU requests (#116)
	spec.GPURequests = gpuRequestsFromCompose(svc.Gpus)

	// Pull strategy (#120) — validator already restricted the enum.
	spec.PullPolicy = svc.PullPolicy

	// Privileged + security_opt (#119) — projected verbatim. Both gated
	// by the validator's label check and by Apply admission against the
	// calling token's allows_privileged flag.
	spec.Privileged = svc.Privileged
	spec.SecurityOpt = cloneStringList(svc.SecurityOpt)
	// network_mode (#121) — projected verbatim. Validator already
	// restricted the value to "", "none", or "service:<name>"; the
	// lifecycle layer resolves `service:<name>` to a docker
	// `container:<id>` lazily at create time so the target's currently-
	// running container id is reflected on every reconcile.
	spec.NetworkMode = svc.NetworkMode


	return spec
}

// resolvedResources is the small, source-resolved view of a service's CPU and
// memory limits. Fields use the same units the ContainerSpec exposes.
type resolvedResources struct {
	NanoCPUs               int64
	MemoryBytes            int64
	MemoryReservationBytes int64
	CPUShares              int64
	CpusetCpus             string
	PidsLimit              *int64
}

// resolveResources decides UP FRONT which source supplies a service's resource
// limits and reads only that source — it is deliberately NOT a field-by-field
// merge. Compose's modern `deploy.resources` block wins outright whenever it is
// present (any limits or reservations declared); otherwise JACO falls back to
// the legacy top-level keys (`cpus`, `mem_limit`, … — issue #49). cpu_shares /
// cpuset have no modern `deploy.resources` equivalent, so they are read from
// the legacy keys regardless of which path supplied cpus/memory.
func resolveResources(svc types.ServiceConfig) resolvedResources {
	var r resolvedResources

	// cpu_shares and cpuset live only at the top level in compose; carry them
	// through on either path.
	r.CPUShares = svc.CPUShares
	r.CpusetCpus = svc.CPUSet

	if modern := deployResources(svc); modern != nil {
		if lim := modern.Limits; lim != nil {
			r.NanoCPUs = coresToNanoCPUs(float32(lim.NanoCPUs))
			r.MemoryBytes = int64(lim.MemoryBytes)
			r.PidsLimit = positivePidsLimit(lim.Pids)
		}
		if res := modern.Reservations; res != nil {
			r.MemoryReservationBytes = int64(res.MemoryBytes)
		}
		return r
	}

	// Legacy top-level keys.
	r.NanoCPUs = coresToNanoCPUs(svc.CPUS)
	r.MemoryBytes = int64(svc.MemLimit)
	r.MemoryReservationBytes = int64(svc.MemReservation)
	r.PidsLimit = positivePidsLimit(svc.PidsLimit)
	return r
}

// deployResources returns the service's modern resource block when it carries
// any limits or reservations, or nil so the resolver falls back to legacy keys.
func deployResources(svc types.ServiceConfig) *types.Resources {
	if svc.Deploy == nil {
		return nil
	}
	res := svc.Deploy.Resources
	if res.Limits == nil && res.Reservations == nil {
		return nil
	}
	return &res
}

// coresToNanoCPUs converts compose's cores (e.g. 1.5) into docker's NanoCPUs
// quota (cores × 1e9). Zero/negative stays zero so docker applies no limit.
func coresToNanoCPUs(cores float32) int64 {
	if cores <= 0 {
		return 0
	}
	return int64(float64(cores) * 1e9)
}

// positivePidsLimit returns a pointer only when a positive pids limit was
// declared; otherwise nil so docker treats it as "no change".
func positivePidsLimit(pids int64) *int64 {
	if pids <= 0 {
		return nil
	}
	v := pids
	return &v
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

// logConfigFromCompose resolves a service's container log configuration from
// the modern compose `logging:` block (driver + options). Returns nil when the
// service declares no logging so docker's default driver applies. Only the
// modern block is supported — compose-go's loader rejects the legacy top-level
// `log_driver`/`log_opt` keys before they could ever reach JACO.
func logConfigFromCompose(logging *types.LoggingConfig) *LogConfig {
	if logging == nil || (logging.Driver == "" && len(logging.Options) == 0) {
		return nil
	}
	return &LogConfig{
		Driver:  logging.Driver,
		Options: mapStringToMap(types.Mapping(logging.Options)),
	}
}

func cloneStringList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// devicesFromCompose projects compose's []DeviceMapping into the spec's
// shape (issue #115). compose-go has already parsed both short-form
// `"/dev/foo:/dev/foo:rwm"` and long-form maps into the same struct, so
// this is a structural copy. Returns nil for an empty input so the spec
// stays free of empty-slice noise (matters for golden-file tests).
func devicesFromCompose(in []types.DeviceMapping) []DeviceMapping {
	if len(in) == 0 {
		return nil
	}
	out := make([]DeviceMapping, len(in))
	for i, d := range in {
		out[i] = DeviceMapping{
			Source:      d.Source,
			Target:      d.Target,
			Permissions: d.Permissions,
		}
	}
	return out
}

// gpuRequestsFromCompose projects compose's []DeviceRequest (the `gpus:`
// long-form, after compose-go has expanded `gpus: all` into a single
// {Count: -1, Capabilities: ["gpu"]} entry) into the spec shape
// (issue #116). Capabilities arrives as a flat AND list; the runtime
// wraps it for docker's OR-of-AND shape at HostConfig build time.
func gpuRequestsFromCompose(in []types.DeviceRequest) []GPURequest {
	if len(in) == 0 {
		return nil
	}
	out := make([]GPURequest, len(in))
	for i, r := range in {
		out[i] = GPURequest{
			Driver:       r.Driver,
			Count:        int64(r.Count),
			DeviceIDs:    cloneStringList(r.IDs),
			Capabilities: cloneStringList(r.Capabilities),
			Options:      mapStringToMap(r.Options),
		}
	}
	return out
}
