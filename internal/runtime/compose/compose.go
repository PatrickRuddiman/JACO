package compose

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// envFileUnresolvedErrCode is the stable error Code surfaced when a daemon-side
// LoadBytes call receives a compose file that still carries `env_file:`
// entries — env_file must be resolved by the CLI before the bytes ever reach
// the daemon (the daemon node does not have the operator's files on disk).
const envFileUnresolvedErrCode = "env_file_unresolved"

// Load reads a compose file at path and returns the parsed Project plus the
// raw bytes (so Validate can do a strict closed-field-set check without
// re-reading the file from disk).
func Load(path string) (*types.Project, []byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	project, err := loadBytes(filepath.Dir(path), filepath.Base(path), body, false)
	if err != nil {
		return nil, nil, err
	}
	return project, body, nil
}

// LoadBytes parses a compose document supplied as raw bytes (the in-flight
// Deploy.Apply path receives YAML over gRPC — no file on disk).
//
// Daemon-side semantic: any `env_file:` entry is rejected with a stable
// env_file_unresolved ValidationError. The CLI MUST call ResolveEnvFiles
// before sending so the merged values travel as plain `environment:` and the
// daemon never needs the operator's local filesystem.
func LoadBytes(body []byte, virtualFilename string) (*types.Project, error) {
	return loadBytes(".", virtualFilename, body, true)
}

// loadBytes drives the compose-go loader. When rejectEnvFiles is set, a
// pre-pass over the raw YAML rejects any service-level `env_file:` with a
// typed ValidationError before compose-go would try (and fail with an opaque
// "env file ... not found") on the daemon-side WorkingDir=".".
func loadBytes(workingDir, filename string, body []byte, rejectEnvFiles bool) (*types.Project, error) {
	if rejectEnvFiles {
		if name, ok := firstServiceWithEnvFile(body); ok {
			return nil, &ValidationError{
				Code: envFileUnresolvedErrCode,
				Message: fmt.Sprintf(
					"service %q uses env_file; env_file must be resolved client-side before apply",
					name),
				Details: map[string]string{"service": name},
			}
		}
	}
	details := types.ConfigDetails{
		WorkingDir: workingDir,
		ConfigFiles: []types.ConfigFile{{
			Filename: filename,
			Content:  body,
		}},
		Environment: map[string]string{},
	}
	project, err := loader.LoadWithContext(context.Background(), details, func(opts *loader.Options) {
		opts.SetProjectName("jaco", true)
		// We do our own field-level validation; let compose-go normalize but
		// skip its consistency check which can reject otherwise-valid graphs.
		opts.SkipConsistencyCheck = true
		// Bytes have already been fully interpolated CLI-side (jaco apply
		// runs compose.SubstituteEnvVars before Apply, with the operator's
		// top-level `environment:` file as the source). The daemon must NOT
		// re-interpolate against its always-empty Environment map (issue
		// #149): (a) it would emit "variable not set" warnings on every
		// reconcile tick for legitimate `$VAR` shell-escapes in healthcheck
		// commands etc., and (b) it would corrupt those escapes — the CLI
		// already collapsed `$$VAR` to `$VAR` per compose-spec; re-running
		// the substitution treats `$VAR` as a reference and resolves it
		// to "".
		opts.SkipInterpolation = true
	})
	if err != nil {
		if verr := translateLegacyComposeError(err); verr != nil {
			return nil, verr
		}
		return nil, fmt.Errorf("compose load: %w", err)
	}
	return project, nil
}

// ResolveEnvFiles merges every service's `env_file:` entries into its
// `environment:` map and re-emits the compose YAML with `env_file:` stripped.
// This runs CLIENT-SIDE before bytes cross the wire: daemon nodes do not have
// the operator's local env files on disk, so resolution at apply time on the
// daemon is impossible (issue #103).
//
// baseDir is the directory the relative env_file paths resolve against — the
// CLI passes filepath.Dir(composePath).
//
// Precedence is compose-spec semantics, owned end-to-end by compose-go's
// types.Project.WithServicesEnvironmentResolved: later env_file entries
// override earlier ones, and an explicit `environment:` value always wins
// over anything loaded from env_file. Variables with no value
// ("FOO:" — inherit-from-process-env) round-trip through as YAML null and
// reach the runtime as `FOO=`.
//
// Fast path: when no service declares env_file, the raw bytes are returned
// unchanged so the YAML reaches the daemon byte-for-byte identical to what
// the operator wrote.
func ResolveEnvFiles(rawCompose []byte, baseDir string) ([]byte, error) {
	// Locate services that actually carry env_file; if none, do nothing.
	servicesWithEnvFile, err := collectServicesWithEnvFile(rawCompose)
	if err != nil {
		return nil, err
	}
	if len(servicesWithEnvFile) == 0 {
		return rawCompose, nil
	}

	// Drive compose-go end-to-end so precedence (later-file-wins, then
	// environment-wins) and interpolation match the daemon-side loader bit
	// for bit. WithDiscardEnvFiles asks the loader to fold every env_file
	// into Service.Environment for us.
	project, err := loadBytes(baseDir, "compose.yml", rawCompose, false)
	if err != nil {
		return nil, fmt.Errorf("resolve env_file: %w", err)
	}
	resolved := make(map[string]types.MappingWithEquals, len(project.Services))
	for _, svc := range project.Services {
		resolved[svc.Name] = svc.Environment
	}

	// Decode the raw YAML into a node tree so the surgery is local: only
	// `env_file:` and `environment:` move, every other byte the operator
	// wrote (comments, key order, anchors) survives unchanged.
	var doc yaml.Node
	if err := yaml.Unmarshal(rawCompose, &doc); err != nil {
		return nil, fmt.Errorf("resolve env_file: parse yaml: %w", err)
	}
	if err := rewriteEnvFiles(&doc, servicesWithEnvFile, resolved); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("resolve env_file: encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("resolve env_file: encode yaml: %w", err)
	}
	return buf.Bytes(), nil
}

// firstServiceWithEnvFile returns the name of the first service that carries
// an `env_file:` entry, or "" if none does. Tolerates malformed YAML by
// returning ("", false) — the subsequent compose-go load will produce the
// authoritative parse error.
func firstServiceWithEnvFile(body []byte) (string, bool) {
	names, err := collectServicesWithEnvFile(body)
	if err != nil || len(names) == 0 {
		return "", false
	}
	return names[0], true
}

// collectServicesWithEnvFile returns the sorted set of service names that
// declare `env_file:`. Order is deterministic so callers (error messages,
// rewrites) behave reproducibly across runs.
func collectServicesWithEnvFile(body []byte) ([]string, error) {
	var probe struct {
		Services map[string]struct {
			EnvFile yaml.Node `yaml:"env_file"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("parse compose yaml: %w", err)
	}
	var names []string
	for name, svc := range probe.Services {
		if !svc.EnvFile.IsZero() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// rewriteEnvFiles is the in-place node surgery: for each service in `targets`
// it replaces (or inserts) `environment:` with the resolved mapping, and
// removes `env_file:`. Services not in `targets` are untouched.
func rewriteEnvFiles(doc *yaml.Node, targets []string, resolved map[string]types.MappingWithEquals) error {
	root := doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return fmt.Errorf("resolve env_file: empty document")
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("resolve env_file: compose root is not a mapping")
	}
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return fmt.Errorf("resolve env_file: top-level services: missing or not a mapping")
	}
	wanted := make(map[string]bool, len(targets))
	for _, n := range targets {
		wanted[n] = true
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		nameNode := services.Content[i]
		svcNode := services.Content[i+1]
		if !wanted[nameNode.Value] {
			continue
		}
		if svcNode.Kind != yaml.MappingNode {
			// `services: { web: }` — null body; nothing to merge into and
			// nothing for the daemon to honor either. Leave as-is so the
			// loader produces its own error downstream.
			continue
		}
		envMapping := resolved[nameNode.Value]
		if err := setEnvironment(svcNode, envMapping); err != nil {
			return err
		}
		removeKey(svcNode, "env_file")
	}
	return nil
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// removeKey strips key (and its value) from a mapping node. No-op if absent.
func removeKey(m *yaml.Node, key string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// setEnvironment installs the resolved environment mapping under
// `environment:` on svc, replacing any existing value. Keys are emitted
// sorted so callers get deterministic bytes (matters for the daemon's
// envelope hashing and for golden-file testability). Values render as
// double-quoted strings so YAML never coerces "8080" back into an int;
// `nil` MappingWithEquals values (operator wrote `FOO:` with no value) emit
// as YAML null.
func setEnvironment(svc *yaml.Node, env types.MappingWithEquals) error {
	envNode := mappingValue(svc, "environment")
	if envNode == nil {
		envNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		svc.Content = append(svc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "environment"},
			envNode,
		)
	} else {
		envNode.Kind = yaml.MappingNode
		envNode.Tag = "!!map"
		envNode.Style = 0
		envNode.Value = ""
		envNode.Content = envNode.Content[:0]
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}
		var valNode *yaml.Node
		if v := env[k]; v != nil {
			valNode = &yaml.Node{
				Kind:  yaml.ScalarNode,
				Tag:   "!!str",
				Style: yaml.DoubleQuotedStyle,
				Value: *v,
			}
		} else {
			valNode = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
		}
		envNode.Content = append(envNode.Content, keyNode, valNode)
	}
	return nil
}

// legacyComposeFieldEquivalents maps v1/v2 compose keys the modern spec
// dropped to the modern equivalent JACO operators should use. compose-go's
// loader rejects these outright with "additional properties 'X' not
// allowed" before JACO's own validator sees them; translateLegacyComposeError
// detects that pattern and re-emits a typed ValidationError naming the
// equivalent (issue #122).
var legacyComposeFieldEquivalents = map[string]string{
	"log_driver":    "logging.driver",
	"log_opt":       "logging.options",
	"net":           "network_mode",
	"volume_driver": "no direct equivalent; use long-form `volumes:` with `driver:`",
	"dockerfile":    "build.dockerfile",
}

// legacyAdditionalPropertyPattern matches compose-go's stable rejection
// string for unknown fields, capturing the offending key.
var legacyAdditionalPropertyPattern = regexp.MustCompile(`additional properties '([^']+)' not allowed`)

// translateLegacyComposeError inspects a compose-go load error and, when it
// matches the "additional properties 'X' not allowed" shape for a v1/v2
// legacy field, returns a typed ValidationError naming the modern
// equivalent. Returns nil when the error doesn't match — caller falls
// back to the generic "compose load" wrap.
func translateLegacyComposeError(err error) *ValidationError {
	if err == nil {
		return nil
	}
	match := legacyAdditionalPropertyPattern.FindStringSubmatch(err.Error())
	if len(match) != 2 {
		return nil
	}
	field := match[1]
	modern, ok := legacyComposeFieldEquivalents[field]
	if !ok {
		return nil
	}
	return &ValidationError{
		Code: "legacy_compose_field",
		Message: fmt.Sprintf(
			"compose field %q is a v1/v2 spelling dropped from the modern spec; use %s instead",
			field, modern),
		Details: map[string]string{
			"field":             field,
			"modern_equivalent": modern,
		},
	}
}
