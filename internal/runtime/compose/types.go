// Package compose loads compose files, validates them against JACO's closed
// field set (see spec.md §3 In), and projects each service into a
// docker-engine-friendly ContainerSpec for the runtime slice.
package compose

import "time"

// ContainerSpec is the moby-friendly view of one replica's container. The
// runtime/lifecycle package translates this into docker.ContainerCreate +
// HostConfig calls (task 17).
type ContainerSpec struct {
	// Identity / JACO labels
	ClusterID    string
	Deployment   string
	Service      string
	ReplicaID    string
	ReplicaIndex int
	RaftIndex    uint64

	// Image + process
	Image      string
	Command    []string
	Entrypoint []string
	Env        []string // KEY=value strings, sorted
	WorkingDir string
	User       string

	// Labels = user labels + JACO labels, JACO labels always win on conflict.
	Labels map[string]string

	// Mounts + tmpfs
	Mounts []Mount
	Tmpfs  []string

	// Capabilities + sysctls + ulimits
	CapAdd   []string
	CapDrop  []string
	Sysctls  map[string]string
	Ulimits  map[string]Ulimit
	ReadOnly bool

	// LogConfig is the container log driver + options projected from compose's
	// modern `logging:` block (or the legacy top-level `log_driver`/`log_opt`
	// keys — issue #94). Nil means "unset" so docker's default log driver
	// applies. The runtime re-projects this into docker's container.LogConfig.
	LogConfig *LogConfig

	// Resource limits (issue #49). Resolved from either compose's modern
	// `deploy.resources.{limits,reservations}` block or the legacy top-level
	// keys (modern wins when both are present — see resolveResources). Every
	// host that runs a replica re-projects these into docker's
	// container.Resources, so enforcement is per-replica on whichever node
	// hosts the container. Zero values mean "unset" (docker default applies).
	NanoCPUs               int64  // CPU quota in units of 1e-9 cores (cores × 1e9)
	MemoryBytes            int64  // hard memory limit, in bytes
	MemoryReservationBytes int64  // soft memory limit (reservation), in bytes
	CPUShares              int64  // relative CPU weight vs. other containers
	CpusetCpus             string // CPUs the container may run on, e.g. "0-2" or "0,1"
	// PidsLimit caps the number of processes. A nil pointer means "no change"
	// (docker's default); only set when a positive value was declared so we
	// never accidentally pin a container to an unlimited/zero pids cgroup.
	PidsLimit *int64

	// Health
	Healthcheck *Healthcheck

	// Networking: docker network names this container attaches to. Order is
	// the compose-declared order (matters for resolver fallthrough — see
	// discovery slice §3 decision 9).
	Networks []string

	// DNSServers is the per-bridge gateway IPs the runtime reconciler
	// computes from state.Subnets so the container's /etc/resolv.conf
	// points at JACO's DNS Manager (task 27 + iter 31). Empty when the
	// runtime hasn't resolved them yet (test paths) — docker's default
	// applies in that case.
	DNSServers []string

	// Ports the user declared in compose. JACO does NOT publish these to the
	// host (ingress handles that via Caddy). Carried here for audit / docs.
	Ports []PortDecl

	// Shutdown semantics (issue #114). StopSignal/StopGracePeriodSeconds
	// flow through docker's Config.StopSignal/StopTimeout, so they are
	// persisted on the container and honored by `docker stop` regardless of
	// who initiates the stop. Empty/nil mean "docker default" (SIGTERM,
	// 10s).
	StopSignal             string
	StopGracePeriodSeconds *int

	// User+DNS+host knobs (issue #117). All zero values mean "docker
	// default applies" (no override emitted to the engine).
	Hostname   string
	Domainname string
	ExtraHosts []string // "host:ip" entries appended to /etc/hosts
	DNS        []string // overrides the runtime-resolved DNSServers when non-empty
	DNSSearch  []string
	DNSOptions []string
	Init       *bool   // nil = docker default; non-nil = explicit override
	ShmSizeBytes int64  // 0 = docker default

	// Namespace knobs (issue #118). Strings forwarded verbatim into the
	// matching HostConfig modes; empty string means "docker default".
	IpcMode       string
	PidMode       string
	UTSMode       string
	UsernsMode    string
	CgroupnsMode  string // compose `cgroup:` (host|private)
	CgroupParent  string

	// Host device bind-mounts (issue #115). Each entry maps directly to a
	// docker DeviceMapping (PathOnHost / PathInContainer / CgroupPermissions).
	// Empty slice = no devices forwarded; preserves docker default.
	Devices []DeviceMapping

	// Modern GPU requests (issue #116). Maps onto docker's
	// HostConfig.DeviceRequests. Empty = no GPU request emitted, so the
	// daemon falls through to its CPU-only default.
	GPURequests []GPURequest

	// PullPolicy is the per-service pull strategy (issue #120). Empty
	// means "use JACO's default" (always call ImagePull, manifest-check).
	// Validator restricts the value to the closed enum {always, missing,
	// never, build}; the runtime collapses always/missing/build into the
	// existing pull path and short-circuits only on never.
	PullPolicy string

	// Privileged grants the container the host's full kernel surface
	// (issue #119). False by default. Apply admission gates this on the
	// calling token's `allows_privileged` flag; the validator gates it
	// on the service-level `jaco.io/allow-privileged: "true"` label.
	Privileged bool

	// SecurityOpt is the verbatim list of `--security-opt` strings the
	// operator declared (e.g. `seccomp=unconfined`, `apparmor=unconfined`).
	// Same gating as Privileged. Empty = no override; docker applies the
	// daemon-default security profile.
	SecurityOpt []string
	// NetworkMode is the verbatim compose `network_mode:` value (issue #121).
	// Validator restricts it to: "" (default — per-deployment bridge),
	// "none", or "service:<name>" where <name> is another service in the
	// same deployment. The lifecycle layer resolves `service:<name>` to a
	// docker `container:<id>` at container-create time (lazy, retry-able);
	// it is NOT resolved here.
	NetworkMode string

}

// Mount is a single bind / named-volume / tmpfs attachment.
type Mount struct {
	Type     string // "bind" | "volume"
	Source   string // host path for bind, volume name for volume
	Target   string // path inside the container
	ReadOnly bool
}

// DeviceMapping is a single host→container device bind-mount (issue #115).
// Source/Target/CgroupPermissions map directly to docker's
// container.DeviceMapping fields. Permissions follows docker's "rwm"
// convention; empty preserves docker's default.
type DeviceMapping struct {
	Source      string
	Target      string
	Permissions string
}

// GPURequest mirrors a compose `gpus:` entry (issue #116). Count<0 encodes
// "all" (compose's `gpus: all`). Capabilities is a single AND list (every
// device must support every listed cap); the runtime wraps it as
// docker's [][]string OR-of-AND form. Driver/DeviceIDs/Options map verbatim.
type GPURequest struct {
	Driver       string
	Count        int64
	DeviceIDs    []string
	Capabilities []string
	Options      map[string]string
}

// Ulimit covers a single soft/hard pair (e.g. nofile, nproc).
type Ulimit struct {
	Soft int64
	Hard int64
}

// LogConfig mirrors docker's container.LogConfig (driver + options).
type LogConfig struct {
	Driver  string            // e.g. "json-file", "journald", "fluentd"
	Options map[string]string // e.g. {"tag": "...", "max-size": "10m", "max-file": "3"}
}

// PortDecl mirrors a compose `ports:` entry. JACO never publishes the port to
// the host — kept for documentation / inspection only.
type PortDecl struct {
	Container int
	Host      int    // 0 when compose used `8080` without an explicit host side
	Protocol  string // "tcp" | "udp"
}

// Healthcheck mirrors docker's container health spec.
type Healthcheck struct {
	Test        []string
	Interval    time.Duration
	Timeout     time.Duration
	Retries     int
	StartPeriod time.Duration
}

// SpecOptions are the bits the caller must supply when projecting a compose
// service into a per-replica ContainerSpec.
type SpecOptions struct {
	ClusterID    string
	Deployment   string
	Service      string
	ReplicaID    string
	ReplicaIndex int
	RaftIndex    uint64
}

// ValidationError is the typed result Validate returns when the compose file
// reaches outside the JACO-supported closed set. Code matches the pb.Error
// codes consumed by Deploy.Apply (task 14).
type ValidationError struct {
	Code    string
	Message string
	Details map[string]string
}

// Error implements the error interface.
func (e *ValidationError) Error() string { return e.Message }
