package grpcsrv

import (
	"fmt"
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
type JacoYAML struct {
	Deployment string            `yaml:"deployment"`
	Services   []JacoServiceDecl `yaml:"services"`
	Routes     []JacoRouteDecl   `yaml:"routes"`
	// ACME is the deployment-level ACME switch. "off" disables automatic TLS
	// for every route in this deployment unless a route sets its own
	// `tls:` explicitly. Empty / "on" (default) leaves each route's tls
	// decision to the route itself (issue #41). It is a convenience opt-out
	// for dev/internal deployments that don't want JACO racing the operator
	// to Let's Encrypt.
	ACME string `yaml:"acme"`
}

// JacoServiceDecl is one service entry. Name must equal the corresponding
// service key in the adjacent compose file. Replicas is the desired count;
// Placement picks the scheduler mode (spread / pack / hosts) and Hosts pins
// targets when Placement=hosts.
type JacoServiceDecl struct {
	Name      string   `yaml:"name"`
	Replicas  int      `yaml:"replicas"`
	Placement string   `yaml:"placement"`
	Hosts     []string `yaml:"hosts"`
	Networks  []string `yaml:"networks"`
}

// JacoRouteDecl is one Caddy-served HTTP(S) route.
type JacoRouteDecl struct {
	Domain  string `yaml:"domain"`
	Service string `yaml:"service"`
	Port    int    `yaml:"port"`
	TLS     string `yaml:"tls"`            // "auto" (default) | "off"
	Path    string `yaml:"path,omitempty"` // optional URL path prefix; "" = catch-all
}

// ParseJacoYAML unmarshals the manifest and applies defaults (Placement spread,
// TLS auto). Returns a typed JacoYAML; validation against the compose file
// happens in validateJacoYAML.
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

// validateJacoYAML checks intrinsic invariants (non-empty deployment, services
// have valid placement, routes reference declared services). Returns the same
// shape as compose's ValidationError so callers can wrap uniformly.
func validateJacoYAML(j *JacoYAML) (code string, message string, ok bool) {
	if j.Deployment == "" {
		return "validation_failed", "deployment name is required", false
	}
	switch strings.ToLower(j.ACME) {
	case "", "on", "off":
	default:
		return "validation_failed", fmt.Sprintf("acme %q is unknown (want on|off)", j.ACME), false
	}
	if len(j.Services) == 0 {
		return "validation_failed", "at least one service is required", false
	}
	serviceNames := map[string]bool{}
	for _, s := range j.Services {
		if s.Name == "" {
			return "validation_failed", "service name is required", false
		}
		if s.Replicas < 0 {
			return "validation_failed", fmt.Sprintf("service %q replicas must be >= 0", s.Name), false
		}
		switch s.Placement {
		case "spread", "pack", "hosts", "global":
		default:
			return "validation_failed", fmt.Sprintf("service %q has unknown placement %q (want spread|pack|hosts|global)", s.Name, s.Placement), false
		}
		// `placement: global` (daemonset) runs one replica per ready node;
		// `replicas:` is ignored (not rejected). The scheduler logs the
		// override at reconcile time; validation has no warning channel.
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
		if !serviceNames[r.Service] {
			return "validation_failed", fmt.Sprintf("route %q references unknown service %q", r.Domain, r.Service), false
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

// toServiceSpecs converts JacoServiceDecls into pb.ServiceSpecs. Caller has
// already validated.
func toServiceSpecs(decls []JacoServiceDecl) []*pb.ServiceSpec {
	out := make([]*pb.ServiceSpec, 0, len(decls))
	for _, d := range decls {
		out = append(out, &pb.ServiceSpec{
			Name:      d.Name,
			Replicas:  int32(d.Replicas),
			Placement: placementToProto(d.Placement),
			Hosts:     append([]string(nil), d.Hosts...),
			Networks:  append([]string(nil), d.Networks...),
		})
	}
	return out
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
