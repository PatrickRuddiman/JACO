package compose

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// allowedServiceFields is the closed set of compose fields JACO honors per
// spec.md §3 In. Anything else in a service block triggers ValidationError.
var allowedServiceFields = map[string]bool{
	"image":       true,
	"command":     true,
	"entrypoint":  true,
	"environment": true,
	"env_file":    true,
	"volumes":     true,
	"ports":       true,
	"depends_on":  true,
	"healthcheck": true,
	"labels":      true,
	"user":        true,
	"working_dir": true,
	"tmpfs":       true,
	"cap_add":     true,
	"cap_drop":    true,
	"sysctls":     true,
	"ulimits":     true,
	"read_only":   true,
	"networks":    true,

	// `restart` is parsed but explicitly ignored by JACO (the scheduler owns
	// restart decisions). Allowing it here means compose authors can keep
	// `restart: unless-stopped` for documentation without tripping
	// validation; the runtime drops it on container create.
	"restart": true,

	// `build` is parsed but explicitly ignored by JACO — we never build
	// images; they're pulled from a registry the runtime can reach. Accepting
	// the field lets a single compose file serve both `docker compose build`
	// (the developer's workflow) and `jaco apply` without forcing two files.
	"build": true,

	// `name` is harmless — compose lets you set a container name; JACO
	// overrides it with the replica id, but accepting it as input is fine.
	"name": true,
}

// Validate walks the raw compose YAML and rejects any service-level field
// outside allowedServiceFields, plus any service-level network reference
// that doesn't have a matching top-level networks: entry (the implicit
// `_default` network is always considered declared).
func Validate(rawYAML []byte) error {
	var doc rawCompose
	if err := yaml.Unmarshal(rawYAML, &doc); err != nil {
		return fmt.Errorf("parse compose yaml: %w", err)
	}

	declared := map[string]bool{"_default": true}
	for name := range doc.Networks {
		declared[name] = true
	}

	// Sort service names so the first violation is deterministic.
	svcNames := make([]string, 0, len(doc.Services))
	for n := range doc.Services {
		svcNames = append(svcNames, n)
	}
	sort.Strings(svcNames)

	for _, svcName := range svcNames {
		svc := doc.Services[svcName]
		fields := sortedKeys(svc)
		for _, field := range fields {
			if !allowedServiceFields[field] {
				return &ValidationError{
					Code: "validation_failed",
					Message: fmt.Sprintf("service %q uses unsupported compose field %q (not in JACO's closed set)",
						svcName, field),
					Details: map[string]string{
						"service": svcName,
						"field":   field,
					},
				}
			}
		}
		if nets, ok := svc["networks"]; ok {
			for _, n := range networkNames(nets) {
				if !declared[n] {
					return &ValidationError{
						Code: "unknown_network",
						Message: fmt.Sprintf("unknown network: %s; declared: [%s]",
							n, strings.Join(sortedNetworkKeys(declared), ", ")),
						Details: map[string]string{
							"service": svcName,
							"network": n,
						},
					}
				}
			}
		}
	}
	return nil
}

// rawCompose is the strict-key view we use for closed-field validation.
type rawCompose struct {
	Services map[string]map[string]any `yaml:"services"`
	Networks map[string]any            `yaml:"networks"`
	Volumes  map[string]any            `yaml:"volumes"`
}

// networkNames extracts the names from a compose service's `networks:` field,
// which may be either a list (`[frontend, backend]`) or a map
// (`{frontend: {...}, backend: {...}}`).
func networkNames(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case map[string]any:
		out := make([]string, 0, len(t))
		for k := range t {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	case map[any]any:
		out := make([]string, 0, len(t))
		for k := range t {
			if s, ok := k.(string); ok {
				out = append(out, s)
			}
		}
		sort.Strings(out)
		return out
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedNetworkKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
