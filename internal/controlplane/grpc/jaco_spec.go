package grpcsrv

import (
	"fmt"
	"net/mail"
	"sort"
	"strconv"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// JacoYAML is the operator-facing deployment manifest. Pairs with a compose
// file that defines the actual container shapes. Schema is intentionally
// minimal — the moving parts (replicas, placement, ingress routes) live
// here; the heavy compose semantics stay in the compose file.
//
// `services:` is optional and may be partial — issue #99. Compose is the
// source of truth for the container set; entries under `services:` are
// per-service *overrides* (replicas, placement, networks, host pinning).
// Compose services with no JACO entry get a single-replica spread default
// at apply time via MergeServiceDefaults.
type JacoYAML struct {
	Deployment string            `yaml:"deployment"`
	Services   []JacoServiceDecl `yaml:"services,omitempty"`
	Routes     []JacoRouteDecl   `yaml:"routes"`
	// ACME is the deployment-level ACME switch. "off" disables automatic TLS
	// for every route in this deployment unless a route sets its own
	// `tls:` explicitly. Empty / "on" (default) leaves each route's tls
	// decision to the route itself (issue #41). It is a convenience opt-out
	// for dev/internal deployments that don't want JACO racing the operator
	// to Let's Encrypt.
	ACME string `yaml:"acme"`
	// ACMEEmail is this stack's ACME (Let's Encrypt) contact address.
	// Empty (the default) means fall back to the cluster-wide jacod.yaml
	// `acme_email`. Each stack with a distinct non-empty value gets its
	// own Caddy automation policy and its own ACME account, so renewal
	// notifications reach that stack's owner instead of one global ops
	// inbox (issue #102).
	ACMEEmail string `yaml:"acme_email"`
	// Environment is an optional path to an env-style file whose KEY=value
	// entries fill the compose-spec ${VAR} interpolation environment for the
	// adjacent compose file (issue #?). Resolved CLIENT-SIDE before bytes
	// cross the wire: the daemon never sees the env file or its values
	// separately — the resolved values land baked into ComposeYaml.
	//
	// Path is interpreted relative to the jaco.yaml file's directory (the
	// same convention compose's service-level `env_file:` uses).
	//
	// Distinct from the per-service compose `environment:` map. At the
	// jaco.yaml top level this keyword means "this deployment's env file";
	// the value is a path string, not a mapping. See
	// docs/manifests/jaco-yaml.md for the full discussion.
	Environment string `yaml:"environment,omitempty"`
}

// JacoServiceDecl is one service-override entry. Name must equal a service
// key in the adjacent compose file. Every field except Name is optional;
// any field that is left unset falls back to the compose default (Replicas
// from `deploy.replicas` if present else 1; Networks from the compose
// service's `networks:` map).
//
// Replicas is a pointer so the wire form distinguishes "unset" (use compose
// default) from an explicit zero ("run zero replicas; keep routes/certs
// provisioned" — see docs/manifests/jaco-yaml.md).
type JacoServiceDecl struct {
	Name      string   `yaml:"name"`
	Replicas  *int     `yaml:"replicas,omitempty"`
	Placement string   `yaml:"placement,omitempty"`
	Hosts     []string `yaml:"hosts,omitempty"`
	Networks  []string `yaml:"networks,omitempty"`
}

// JacoRouteDecl is one Caddy-served HTTP(S) route.
type JacoRouteDecl struct {
	Domain    string `yaml:"domain"`
	Service   string `yaml:"service"`
	Port      int    `yaml:"port"`
	TLS       string `yaml:"tls"`                  // "auto" (default) | "off"
	Path      string `yaml:"path,omitempty"`       // optional URL path prefix; "" = catch-all
	StripPath bool   `yaml:"strip_path,omitempty"` // strip the matched path prefix before proxying
}

// ParseJacoYAML unmarshals the manifest and applies defaults (Placement spread
// on entries that *do* set Replicas/Hosts/Networks but omit Placement, TLS
// auto unless deployment-level acme:off). Returns a typed JacoYAML;
// validation against the compose file happens in validateJacoYAML.
//
// It rejects input that contains compose_service keys: the field was removed
// pre-1.0 and name is now the single source of truth. Yaml unmarshal failures
// are returned as *ValidationError with Code="parse_failed" so callers can
// errors.As and surface a stable code without re-parsing the message.
func ParseJacoYAML(body []byte) (*JacoYAML, error) {
	// Pre-check: reject compose_service before struct decode so the error
	// message is clear regardless of struct tag changes. Walk the entire
	// decoded tree, not just services[*], so the field is caught no matter
	// where a user tucks it (top-level, nested subtree, YAML merge target).
	var raw any
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse jaco yaml: %w", err)
	}
	if containsKey(raw, "compose_service") {
		return nil, fmt.Errorf("compose_service is no longer supported; rename \"name\" to match the compose service key")
	}

	var j JacoYAML
	if err := yaml.Unmarshal(body, &j); err != nil {
		return nil, &ValidationError{
			Code:    "parse_failed",
			Message: fmt.Sprintf("parse jaco yaml: %s", err.Error()),
		}
	}
	for i := range j.Services {
		if j.Services[i].Placement == "" {
			j.Services[i].Placement = "spread"
		}
	}
	// Deployment-level `acme: off` (issue #41) implicitly disables TLS on
	// every route that didn't set `tls:` explicitly. A route may still
	// override with `tls: auto`. This runs BEFORE the default-to-auto step
	// so a blank route under `acme: off` resolves to off, not auto.
	deploymentACMEOff := strings.EqualFold(j.ACME, "off")
	for i := range j.Routes {
		if j.Routes[i].TLS == "" {
			if deploymentACMEOff {
				j.Routes[i].TLS = "off"
			} else {
				j.Routes[i].TLS = "auto"
			}
		}
	}
	return &j, nil
}

// ValidateJacoYAMLBytes parses and validates a jaco YAML manifest from raw
// bytes. Returns a non-nil error if parsing fails or any intrinsic invariant
// is violated. This is the CLI-facing surface for local lint; it calls
// ParseJacoYAML + validateJacoYAML in one step without touching a cluster.
func ValidateJacoYAMLBytes(data []byte) error {
	j, err := ParseJacoYAML(data)
	if err != nil {
		return err
	}
	if code, msg, ok := validateJacoYAML(j); !ok {
		return &ValidationError{Code: code, Message: msg}
	}
	return nil
}

// validateJacoYAML checks intrinsic invariants that do not require the
// compose project: a deployment name, valid enum values on every present
// service entry, no duplicate service names, and intrinsic route shape
// (non-empty domain, valid port/tls, no (domain,path) duplicates, no mixed
// TLS modes on one domain).
//
// `services:` may be empty or absent — compose is the source of truth for
// the container set, so a slim jaco.yaml that declares only routes is valid
// (issue #99). Per-route service references can't be validated here because
// they may legitimately point to compose-only services; that check runs at
// Apply time in deploy.go against the merged service set.
func validateJacoYAML(j *JacoYAML) (code string, message string, ok bool) {
	if j.Deployment == "" {
		return "validation_failed", "deployment name is required", false
	}
	switch strings.ToLower(j.ACME) {
	case "", "on", "off":
	default:
		return "validation_failed", fmt.Sprintf("acme %q is unknown (want on|off)", j.ACME), false
	}
	// Syntactic email validation only when ACME is on (i.e. not explicitly
	// "off"; empty defaults to on). A malformed contact would surface as
	// an opaque CA-side rejection mid-issuance; catching it at apply time
	// gives a clean error pointing at the manifest. Empty is allowed and
	// means "fall back to the cluster-wide acme_email" (#102).
	if j.ACMEEmail != "" && !strings.EqualFold(j.ACME, "off") {
		if _, err := mail.ParseAddress(j.ACMEEmail); err != nil {
			return "validation_failed",
				fmt.Sprintf("acme_email %q is not a valid email address: %v", j.ACMEEmail, err),
				false
		}
	}
	serviceNames := map[string]bool{}
	for _, s := range j.Services {
		if s.Name == "" {
			return "validation_failed", "service name is required", false
		}
		if s.Replicas != nil && *s.Replicas < 0 {
			return "validation_failed", fmt.Sprintf("service %q replicas must be >= 0", s.Name), false
		}
		switch s.Placement {
		case "spread", "pack", "hosts", "global":
		default:
			return "validation_failed", fmt.Sprintf("service %q has unknown placement %q (want spread|pack|hosts|global)", s.Name, s.Placement), false
		}
		// `placement: global` (daemonset) runs one replica per ready node;
		// an explicit `replicas:` is meaningless under global and a likely
		// authoring mistake — reject it up front so users notice instead of
		// silently watching the scheduler ignore their count (issue #99).
		// `replicas:` omitted under global is fine: the scheduler derives
		// the count from the ready-node set.
		if s.Placement == "global" && s.Replicas != nil {
			return "validation_failed", fmt.Sprintf("service %q uses placement=global; remove replicas (global runs one replica per ready node)", s.Name), false
		}
		if s.Placement == "hosts" && len(s.Hosts) == 0 {
			return "validation_failed", fmt.Sprintf("service %q uses placement=hosts but hosts is empty", s.Name), false
		}
		if serviceNames[s.Name] {
			return "validation_failed", fmt.Sprintf("service %q declared more than once", s.Name), false
		}
		serviceNames[s.Name] = true
	}
	// (domain, path) is the uniqueness key — Caddy can only dispatch one
	// upstream per request, so any duplicate (regardless of service/port/tls)
	// would silently shadow another route. Reject all duplicates up front.
	type domainPath struct{ domain, path string }
	seenRoutes := map[domainPath]bool{}
	// A domain must pick one TLS mode: Caddy can't half-redirect (some paths
	// 308→https, others served plain). Mixed tls:auto + tls:off on one domain
	// is rejected up front (issue #46). TLS is already resolved to auto/off by
	// ParseJacoYAML (deployment-level acme:off + default-to-auto).
	domainTLS := map[string]string{}
	for _, r := range j.Routes {
		if r.Domain == "" {
			return "validation_failed", "route domain is required", false
		}
		if r.Service == "" {
			return "validation_failed", fmt.Sprintf("route %q service is required", r.Domain), false
		}
		if r.Port <= 0 {
			return "validation_failed", fmt.Sprintf("route %q port must be > 0", r.Domain), false
		}
		switch r.TLS {
		case "auto", "off":
		default:
			return "validation_failed", fmt.Sprintf("route %q has unknown tls %q (want auto|off)", r.Domain, r.TLS), false
		}
		if prev, ok := domainTLS[r.Domain]; ok && prev != r.TLS {
			return "route_tls_mixed", fmt.Sprintf("domain %q mixes tls:auto and tls:off routes; a domain must use a single TLS mode", r.Domain), false
		}
		domainTLS[r.Domain] = r.TLS
		key := domainPath{r.Domain, r.Path}
		if seenRoutes[key] {
			return "validation_failed", fmt.Sprintf("route conflict: domain %q path %q is declared more than once; (domain, path) combinations must be unique", r.Domain, r.Path), false
		}
		seenRoutes[key] = true
	}
	return "", "", true
}

// MergeServiceDefaults produces the final ServiceSpec set by walking the
// compose project (which is the authoritative container set) and folding
// in the per-service overrides declared under jaco.yaml's `services:`
// (issue #99). Output is sorted by service name for deterministic raft
// commands.
//
// Merge rules per service:
//
//   - Replicas: JACO `Replicas` wins when set (including 0). Otherwise
//     compose `deploy.replicas` when set. Otherwise 1.
//   - Placement: JACO `Placement` wins when set. Otherwise "spread".
//   - Hosts: copied from JACO when set; not derivable from compose.
//   - Networks: JACO `Networks` wins when set (non-empty list). Otherwise
//     the alphabetically-sorted keys of compose's per-service `networks:`
//     map. An empty result means "the implicit per-deployment _default"
//     and is interpreted that way by the runtime (see
//     runtime_attach.BridgesForService).
//
// Caller must already have validated both files: every JACO entry's Name
// must resolve to a compose service. Returns an error if a JACO entry
// references a service that isn't in the compose project (defense in depth;
// the deploy.go cross-check fires first).
func MergeServiceDefaults(jaco *JacoYAML, project *composetypes.Project) ([]*pb.ServiceSpec, error) {
	if project == nil {
		return nil, fmt.Errorf("MergeServiceDefaults: compose project is nil")
	}
	overrides := make(map[string]*JacoServiceDecl, len(jaco.Services))
	for i := range jaco.Services {
		s := &jaco.Services[i]
		overrides[s.Name] = s
	}
	for name := range overrides {
		if _, ok := project.Services[name]; !ok {
			return nil, fmt.Errorf("MergeServiceDefaults: jaco service %q has no matching compose service", name)
		}
	}

	names := make([]string, 0, len(project.Services))
	for name := range project.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]*pb.ServiceSpec, 0, len(names))
	for _, name := range names {
		svc := project.Services[name]
		override := overrides[name]
		spec := mergeOne(svc, override)
		out = append(out, spec)
	}
	return out, nil
}

// mergeOne resolves the final ServiceSpec for a single compose service by
// applying override (which may be nil, meaning compose-only) on top of the
// compose defaults.
func mergeOne(svc composetypes.ServiceConfig, override *JacoServiceDecl) *pb.ServiceSpec {
	spec := &pb.ServiceSpec{Name: svc.Name}

	// Replicas: JACO override wins when set; else compose deploy.replicas;
	// else 1. Validation has already guaranteed non-negative.
	switch {
	case override != nil && override.Replicas != nil:
		spec.Replicas = int32(*override.Replicas)
	case svc.Deploy != nil && svc.Deploy.Replicas != nil:
		spec.Replicas = int32(*svc.Deploy.Replicas)
	default:
		spec.Replicas = 1
	}

	// Placement: only JACO supplies this. Default spread for compose-only
	// services.
	placement := "spread"
	if override != nil && override.Placement != "" {
		placement = override.Placement
	}
	spec.Placement = placementToProto(placement)

	// Hosts: only meaningful under placement=hosts; nil otherwise.
	if override != nil && len(override.Hosts) > 0 {
		spec.Hosts = append([]string(nil), override.Hosts...)
	}

	// Networks: JACO override wins when set; else the alphabetically-sorted
	// keys of compose's per-service networks map. Empty result means
	// "implicit _default" and is handled downstream.
	switch {
	case override != nil && len(override.Networks) > 0:
		spec.Networks = append([]string(nil), override.Networks...)
	case len(svc.Networks) > 0:
		nets := make([]string, 0, len(svc.Networks))
		for n := range svc.Networks {
			nets = append(nets, n)
		}
		sort.Strings(nets)
		spec.Networks = nets
	}

	return spec
}

// toTCPRoutes derives the cluster-wide TCP ingress listeners from a compose
// project's published ports. Only entries with a numeric published host port,
// tcp protocol, and no host-IP scoping qualify; bare/ephemeral, UDP, and
// 127.0.0.1-scoped entries are documentation and produce no listener. Output
// is sorted by published port for determinism (the FSM + config builder both
// rely on a stable order). Compose-go has already expanded port ranges into
// individual entries by the time the project reaches here.
func toTCPRoutes(deployment string, project *composetypes.Project) []*pb.TCPRoute {
	if project == nil {
		return nil
	}
	var out []*pb.TCPRoute
	for _, svc := range project.Services {
		for _, p := range svc.Ports {
			if p.Published == "" {
				continue // no host publish — internal/documentation only
			}
			if p.Protocol != "" && p.Protocol != "tcp" {
				continue // UDP etc. out of scope
			}
			if p.HostIP != "" && p.HostIP != "0.0.0.0" {
				continue // scoped to a specific host IP — not cluster-wide ingress
			}
			published, err := strconv.Atoi(p.Published)
			if err != nil || published <= 0 {
				continue
			}
			out = append(out, &pb.TCPRoute{
				PublishedPort: int32(published),
				Deployment:    deployment,
				Service:       svc.Name,
				ContainerPort: int32(p.Target),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetPublishedPort() < out[j].GetPublishedPort() })
	return out
}

// toRoutes converts JacoRouteDecls into pb.Route entries scoped to deployment.
func toRoutes(deployment string, decls []JacoRouteDecl) []*pb.Route {
	out := make([]*pb.Route, 0, len(decls))
	for _, d := range decls {
		out = append(out, &pb.Route{
			Domain:     d.Domain,
			Deployment: deployment,
			Service:    d.Service,
			Port:       int32(d.Port),
			TlsAuto:    d.TLS == "auto",
			Path:       d.Path,
			StripPath:  d.StripPath,
		})
	}
	return out
}

// containsKey recursively walks a decoded YAML tree and reports whether any
// map node in the tree (at any depth) has the given key. Used to catch
// deprecated keys regardless of where the user placed them.
func containsKey(node any, key string) bool {
	switch v := node.(type) {
	case map[string]any:
		if _, ok := v[key]; ok {
			return true
		}
		for _, child := range v {
			if containsKey(child, key) {
				return true
			}
		}
	case map[any]any:
		// yaml.v3 normally decodes to map[string]any, but defensive handling
		// for any code path that lands here with an interface-keyed map.
		for k, child := range v {
			if ks, ok := k.(string); ok && ks == key {
				return true
			}
			if containsKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if containsKey(child, key) {
				return true
			}
		}
	}
	return false
}

func placementToProto(s string) pb.ServiceSpec_PlacementMode {
	switch s {
	case "pack":
		return pb.ServiceSpec_PLACEMENT_MODE_PACK
	case "hosts":
		return pb.ServiceSpec_PLACEMENT_MODE_HOSTS
	case "global":
		return pb.ServiceSpec_PLACEMENT_MODE_GLOBAL
	default:
		return pb.ServiceSpec_PLACEMENT_MODE_SPREAD
	}
}
