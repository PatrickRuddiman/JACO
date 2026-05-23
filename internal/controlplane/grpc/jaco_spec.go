package grpcsrv

import (
	"fmt"

	"gopkg.in/yaml.v3"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// JacoYAML is the operator-facing deployment manifest. Pairs with a compose
// file that defines the actual container shapes. Schema is intentionally
// minimal — the moving parts (replicas, placement, ingress routes) live
// here; the heavy compose semantics stay in the compose file.
type JacoYAML struct {
	Deployment string          `yaml:"deployment"`
	Services   []JacoServiceDecl `yaml:"services"`
	Routes     []JacoRouteDecl   `yaml:"routes"`
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
	TLS     string `yaml:"tls"` // "auto" (default) | "off"
}

// ParseJacoYAML unmarshals the manifest and applies defaults (Placement spread,
// TLS auto). Returns a typed JacoYAML; validation against the compose file
// happens in validateJacoYAML.
//
// It rejects input that contains compose_service keys: the field was removed
// pre-1.0 and name is now the single source of truth.
func ParseJacoYAML(body []byte) (*JacoYAML, error) {
	// Pre-check: reject compose_service before struct decode so the error
	// message is clear regardless of struct tag changes.
	var raw map[string]any
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse jaco yaml: %w", err)
	}
	if svcs, ok := raw["services"]; ok {
		if list, ok := svcs.([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					if _, has := m["compose_service"]; has {
						return nil, fmt.Errorf("compose_service is no longer supported; rename \"name\" to match the compose service key")
					}
				}
			}
		}
	}

	var j JacoYAML
	if err := yaml.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("parse jaco yaml: %w", err)
	}
	for i := range j.Services {
		if j.Services[i].Placement == "" {
			j.Services[i].Placement = "spread"
		}
	}
	for i := range j.Routes {
		if j.Routes[i].TLS == "" {
			j.Routes[i].TLS = "auto"
		}
	}
	return &j, nil
}

// validateJacoYAML checks intrinsic invariants (non-empty deployment, services
// have valid placement, routes reference declared services). Returns the same
// shape as compose's ValidationError so callers can wrap uniformly.
func validateJacoYAML(j *JacoYAML) (code string, message string, ok bool) {
	if j.Deployment == "" {
		return "validation_failed", "deployment name is required", false
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
		case "spread", "pack", "hosts":
		default:
			return "validation_failed", fmt.Sprintf("service %q has unknown placement %q (want spread|pack|hosts)", s.Name, s.Placement), false
		}
		if s.Placement == "hosts" && len(s.Hosts) == 0 {
			return "validation_failed", fmt.Sprintf("service %q uses placement=hosts but hosts is empty", s.Name), false
		}
		if serviceNames[s.Name] {
			return "validation_failed", fmt.Sprintf("service %q declared more than once", s.Name), false
		}
		serviceNames[s.Name] = true
	}
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
		})
	}
	return out
}

func placementToProto(s string) pb.ServiceSpec_PlacementMode {
	switch s {
	case "pack":
		return pb.ServiceSpec_PLACEMENT_MODE_PACK
	case "hosts":
		return pb.ServiceSpec_PLACEMENT_MODE_HOSTS
	default:
		return pb.ServiceSpec_PLACEMENT_MODE_SPREAD
	}
}
