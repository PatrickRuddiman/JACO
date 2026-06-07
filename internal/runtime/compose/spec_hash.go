package compose

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ServiceSpecHash returns the SHA-256 of the canonical `services.<name>`
// subtree of composeBytes. The hash is the reconciler's drift signal
// (issue #148): the scheduler bumps a replica's RaftIndex whenever the
// hash of its service's resolved spec changes, which causes lifecycle.Start
// to recreate the container — container env (and every other compose field)
// is baked at create time and immutable afterward.
//
// Canonical form: decode the yaml.Node into a plain Go value (maps, lists,
// scalars), then json.Marshal it. encoding/json sorts map[string]any keys
// alphabetically, so the byte sequence depends only on the semantic
// content of the service subtree — not on key order, comments, blank
// lines, indent style, or other formatting the operator might tweak.
// Cosmetic edits to the compose file don't trigger unnecessary
// recreations; semantic changes (env values, healthcheck command,
// mounts, labels, …) do. Returns an error when the document doesn't
// carry a top-level `services` mapping, or when the named service is
// absent — both indicate a programming mistake in the caller, not an
// operator-recoverable condition.
func ServiceSpecHash(composeBytes []byte, serviceName string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(composeBytes, &doc); err != nil {
		return nil, fmt.Errorf("ServiceSpecHash: parse yaml: %w", err)
	}
	svcNode, err := findServiceNode(&doc, serviceName)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := svcNode.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("ServiceSpecHash: decode service %q: %w", serviceName, err)
	}
	canonical, err := json.Marshal(coerceStringKeys(decoded))
	if err != nil {
		return nil, fmt.Errorf("ServiceSpecHash: marshal service %q: %w", serviceName, err)
	}
	sum := sha256.Sum256(canonical)
	return sum[:], nil
}

// coerceStringKeys recursively rewrites map[any]any → map[string]any so
// json.Marshal accepts the value and sorts keys alphabetically. yaml.v3
// emits map[string]any for plain string-keyed mappings (the common case);
// this guard catches the rare integer- or bool-keyed mapping a compose
// document might carry (in practice: none, but it's cheap insurance).
func coerceStringKeys(v any) any {
	switch x := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[fmt.Sprint(k)] = coerceStringKeys(val)
		}
		return out
	case map[string]any:
		for k, val := range x {
			x[k] = coerceStringKeys(val)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = coerceStringKeys(val)
		}
		return x
	default:
		return v
	}
}

// findServiceNode walks the document → root mapping → `services` mapping →
// returns the value node bound to serviceName. Returns a typed error for
// each failure mode so callers can distinguish "compose has no services"
// from "service name not in this compose."
func findServiceNode(doc *yaml.Node, serviceName string) (*yaml.Node, error) {
	root := doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil, fmt.Errorf("ServiceSpecHash: empty document")
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("ServiceSpecHash: root is not a mapping (kind=%d)", root.Kind)
	}
	servicesNode := mappingValue(root, "services")
	if servicesNode == nil {
		return nil, fmt.Errorf("ServiceSpecHash: top-level `services:` mapping missing")
	}
	if servicesNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("ServiceSpecHash: `services:` is not a mapping (kind=%d)", servicesNode.Kind)
	}
	svcNode := mappingValue(servicesNode, serviceName)
	if svcNode == nil {
		return nil, fmt.Errorf("ServiceSpecHash: service %q not in compose document", serviceName)
	}
	return svcNode, nil
}
