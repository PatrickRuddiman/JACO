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

	// `logging` is honored: JACO projects the driver + options onto the
	// container's log configuration (issue #94). Only the modern `logging:`
	// block is supported — compose-go's loader rejects the legacy top-level
	// `log_driver`/`log_opt` keys outright, so allowing them here would let
	// Validate pass a manifest that Load then refuses.
	"logging": true,

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

	// Shutdown semantics (issue #114). Both honored: stop_signal sets
	// Config.StopSignal, stop_grace_period sets Config.StopTimeout. Without
	// these, Postgres/Redis/nginx/Kafka get the wrong shutdown signal or
	// not enough time to flush on `jaco rm` / replica rotation — silent
	// data-loss-shaped behavior.
	"stop_signal":        true,
	"stop_grace_period":  true,

	// Trivial HostConfig/Config passthroughs (issue #117). Each maps to
	// one docker field; no JACO semantics layered on top.
	"extra_hosts": true,
	"dns":         true,
	"dns_search":  true,
	"dns_opt":     true,
	"init":        true,
	"shm_size":    true,
	"hostname":    true,
	"domainname":  true,

	// Namespace knobs (issue #118). Each maps to one HostConfig field.
	// `pid: host`, `ipc: host`, `uts: host`, `userns_mode: host` weaken
	// isolation by design; honored as-written (no runtime gate in this
	// PR — operator policy is a separate decision).
	"ipc":           true,
	"pid":           true,
	"uts":           true,
	"userns_mode":   true,
	"cgroup":        true,
	"cgroup_parent": true,

	// Host device bind-mounts (issue #115). Maps to HostConfig.Devices.
	// Grants host-kernel surface; operator policy is a separate decision
	// (docs/concepts/isolation.md mentions the future restriction path).
	"devices": true,

	// Modern GPU request syntax (issue #116). Maps to HostConfig.DeviceRequests.
	// Both `gpus: all` and the long-form list are honored. Requires the
	// operator-managed nvidia-container-runtime (or AMD equivalent) on each
	// node; JACO does not install it.
	"gpus": true,

	// Per-deployment pull strategy override (issue #120). Validator
	// restricts to a closed enum; the runtime treats `always` and
	// `missing` identically (both call ImagePull, which manifest-checks
	// against the registry — cheap when the image is already present),
	// and `never` skips the pull entirely. `build` is accepted but
	// treated as `missing` (JACO never builds). `daily`/`weekly` are
	// rejected.
	"pull_policy": true,

	// Privileged-mode + security-opt overrides (issue #119). Validator
	// requires a matching `labels: { "jaco.io/allow-privileged": "true" }`
	// on the service (defense in depth against typos) and Apply admission
	// additionally requires the calling token to carry
	// `allows_privileged=true` — see internal/controlplane/grpc/deploy.go.
	"privileged":   true,
	"security_opt": true,
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
		if pp, ok := svc["pull_policy"]; ok {
			if verr := checkPullPolicy(svcName, pp); verr != nil {
				return verr
			}
		}
		if verr := checkPrivilegedLabel(svcName, svc); verr != nil {
			return verr
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

// allowedPullPolicies is the closed enum JACO accepts for `pull_policy:`.
// `always` and `missing` both map to the existing "call ImagePull" path
// (the daemon manifest-checks; cheap when up-to-date). `never` is the only
// value that materially changes runtime behavior. `build` is accepted but
// the runtime treats it as `missing` because JACO never builds — see
// internal/runtime/pull/policy.go. `daily` and `weekly` from the compose
// spec are out of scope (issue #120).
var allowedPullPolicies = map[string]bool{
	"always":  true,
	"missing": true,
	"never":   true,
	"build":   true,
}

// checkPullPolicy enforces the closed enum. Anything else (including the
// spec's `daily`/`weekly` cadences) is rejected with a typed error so the
// operator sees a clear refusal instead of silent fallback to the default.
func checkPullPolicy(svcName string, v any) *ValidationError {
	s, ok := v.(string)
	if !ok {
		return &ValidationError{
			Code:    "validation_failed",
			Message: fmt.Sprintf("service %q: pull_policy must be a string", svcName),
			Details: map[string]string{"service": svcName, "field": "pull_policy"},
		}
	}
	if !allowedPullPolicies[s] {
		return &ValidationError{
			Code: "validation_failed",
			Message: fmt.Sprintf("service %q: pull_policy %q unsupported (allowed: always, missing, never, build)",
				svcName, s),
			Details: map[string]string{"service": svcName, "field": "pull_policy", "value": s},
		}
	}
	return nil
}

// privilegedLabelKey is the service-level compose label JACO requires before
// `privileged:` or `security_opt:` are admitted (issue #119). The label MUST
// be `"true"` exactly — anything else (including the docker-compose-style
// bare boolean) reads as not-set, matching how compose serialises labels.
const privilegedLabelKey = "jaco.io/allow-privileged"

// checkPrivilegedLabel runs after the closed-field pass. Returns a typed
// ValidationError when a service sets `privileged: true` or a non-empty
// `security_opt:` list without carrying `labels: { "jaco.io/allow-privileged":
// "true" }`. The token-class gate is enforced one layer up (Apply admission)
// — this check is the pure-YAML half so `jaco validate` catches the typo
// before any wire trip.
func checkPrivilegedLabel(svcName string, svc map[string]any) *ValidationError {
	priv, _ := svc["privileged"].(bool)
	secOpts := sliceLen(svc["security_opt"])
	if !priv && secOpts == 0 {
		return nil
	}
	if hasPrivilegedLabel(svc["labels"]) {
		return nil
	}
	fields := privilegedFieldsCSV(priv, secOpts > 0)
	return &ValidationError{
		Code: "validation_failed",
		Message: fmt.Sprintf("service %q uses %s but lacks required label %s=\"true\"",
			svcName, fields, privilegedLabelKey),
		Details: map[string]string{
			"service": svcName,
			"fields":  fields,
			"label":   privilegedLabelKey,
		},
	}
}

// sliceLen returns the element count when v is a YAML list, or 0 otherwise.
// A nil interface, an empty list, a non-list value all collapse to 0 — the
// privileged gate is "did the operator actually declare any security_opt?".
func sliceLen(v any) int {
	if v == nil {
		return 0
	}
	s, ok := v.([]any)
	if !ok {
		return 0
	}
	return len(s)
}

// hasPrivilegedLabel reports whether the service's `labels:` block carries
// `jaco.io/allow-privileged: "true"`. Compose accepts both the list form
// (`["k=v", …]`) and the map form (`{k: v, …}`); both yield string values
// here. The match is on exact string `"true"` — `True`, `1`, or bare booleans
// do NOT pass, matching how compose serialises label values.
func hasPrivilegedLabel(v any) bool {
	switch labels := v.(type) {
	case map[string]any:
		s, _ := labels[privilegedLabelKey].(string)
		return s == "true"
	case map[any]any:
		s, _ := labels[privilegedLabelKey].(string)
		return s == "true"
	case []any:
		want := privilegedLabelKey + "=true"
		for _, item := range labels {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// privilegedFieldsCSV renders the human-facing list of which gated fields
// this service set, in a deterministic order so the error message is stable
// across runs.
func privilegedFieldsCSV(priv, secOpts bool) string {
	switch {
	case priv && secOpts:
		return "privileged,security_opt"
	case priv:
		return "privileged"
	default:
		return "security_opt"
	}
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
