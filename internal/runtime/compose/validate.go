package compose

import (
	"fmt"
	"sort"
	"strconv"
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

	// `logging` (modern) plus the legacy top-level `log_driver`/`log_opt` keys
	// are honored: JACO projects the driver + options onto the container's
	// log configuration (issue #94). The modern block wins when both are
	// present (see compose.logConfigFromCompose).
	"logging":    true,
	"log_driver": true,
	"log_opt":    true,

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

	// `deploy` is accepted wholesale: JACO reads only its `resources.{limits,
	// reservations}` subtree to enforce per-replica CPU/memory cgroup limits
	// (issue #49). The non-resource subkeys (`replicas`, `placement`,
	// `restart_policy`, `update_config`, …) are parsed-but-ignored, mirroring
	// the top-level `restart:` treatment — the scheduler owns those decisions.
	"deploy": true,

	// Legacy (v2-style) top-level resource keys are accepted as a fallback for
	// compose files that predate `deploy.resources` (issue #49). When both a
	// modern `deploy.resources` block and these legacy keys are present, the
	// modern block wins (see compose.resolveResources).
	"cpus":            true,
	"mem_limit":       true,
	"mem_reservation": true,
	"pids_limit":      true,
	"cpu_shares":      true,
	"cpuset":          true,
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
		if ports, ok := svc["ports"]; ok {
			if verr := checkReservedPorts(svcName, ports); verr != nil {
				return verr
			}
		}
	}
	return nil
}

// reservedHostPorts are the host ports JACO's HTTP/S ingress owns; a compose
// service may not publish them (they'd silently steal Caddy's listeners).
var reservedHostPorts = []int{80, 443}

// checkReservedPorts rejects any ports: entry that publishes a reserved host
// port (80/443). Only the published host side is inspected — container-side
// targets and bare entries with no host publish are documentation and pass.
func checkReservedPorts(svcName string, portsField any) *ValidationError {
	entries, ok := portsField.([]any)
	if !ok {
		return nil
	}
	for _, item := range entries {
		lo, hi, raw, ok := publishedHostRange(item)
		if !ok {
			continue
		}
		for _, rp := range reservedHostPorts {
			if lo <= rp && rp <= hi {
				return &ValidationError{
					Code: "reserved_port",
					Message: fmt.Sprintf("service %q publishes reserved host port %d (entry %q); 80 and 443 belong to JACO's HTTP/S ingress",
						svcName, rp, raw),
					Details: map[string]string{
						"service": svcName,
						"port":    strconv.Itoa(rp),
						"entry":   raw,
					},
				}
			}
		}
	}
	return nil
}

// publishedHostRange extracts the published host port range from one ports:
// entry. Returns ok=false when the entry declares no published host side
// (bare container port, or a long-form map without `published`).
func publishedHostRange(item any) (lo, hi int, raw string, ok bool) {
	switch v := item.(type) {
	case string:
		s := v
		if i := strings.IndexByte(s, '/'); i >= 0 { // drop /tcp|/udp suffix
			s = s[:i]
		}
		parts := strings.Split(s, ":")
		var published string
		switch len(parts) {
		case 2: // "H:C"
			published = parts[0]
		case 3: // "IP:H:C"
			published = parts[1]
		default: // bare "C" — no published host side
			return 0, 0, v, false
		}
		lo, hi, ok = parsePortRange(published)
		return lo, hi, v, ok
	case map[string]any:
		return publishedFromMap(v)
	case map[any]any:
		m := make(map[string]any, len(v))
		for k, val := range v {
			if ks, ok := k.(string); ok {
				m[ks] = val
			}
		}
		return publishedFromMap(m)
	}
	return 0, 0, "", false
}

// publishedFromMap reads the `published` key of a long-form ports: entry.
func publishedFromMap(m map[string]any) (lo, hi int, raw string, ok bool) {
	pub, present := m["published"]
	if !present {
		return 0, 0, "", false
	}
	pubStr := fmt.Sprintf("%v", pub)
	lo, hi, ok = parsePortRange(pubStr)
	return lo, hi, fmt.Sprintf("published: %v", pub), ok
}

// parsePortRange parses "80" → (80,80) or "8000-8100" → (8000,8100).
func parsePortRange(s string) (lo, hi int, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		a, errA := strconv.Atoi(strings.TrimSpace(s[:i]))
		b, errB := strconv.Atoi(strings.TrimSpace(s[i+1:]))
		if errA != nil || errB != nil || a > b {
			return 0, 0, false
		}
		return a, b, true
	}
	p, err := strconv.Atoi(s)
	if err != nil {
		return 0, 0, false
	}
	return p, p, true
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
