package compose

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/compose-spec/compose-go/v2/template"
	"gopkg.in/yaml.v3"
)

// SubstituteEnvVars expands compose-spec ${VAR} interpolation across every
// scalar in a compose document, using env as the variable source. Output is
// the re-encoded YAML bytes; node surgery preserves comments, key order, and
// anchors the same way ResolveEnvFiles does.
//
// env carries the values from the operator's jaco.yaml `environment:` file
// (loaded CLI-side). It is the SOLE source of values — os.Environ() does NOT
// participate, matching JACO's "manifests are explicit and reproducible"
// posture. A nil env behaves as an empty map (defaults in `${VAR:-x}` still
// resolve to "x"; required `${VAR:?msg}` references fail loudly).
//
// Supported syntax (compose-spec parity, owned by compose-go/template):
//
//   - $VAR / ${VAR}                   substitute (missing → empty + warn)
//   - ${VAR:-default} / ${VAR-default} default when unset / unset-or-empty
//   - ${VAR:?msg} / ${VAR?msg}        required (missing → MissingRequiredError)
//   - $$                              literal `$`
//
// Fast path: when the body carries no `$` byte, no interpolation site can
// possibly fire — return the input verbatim so the YAML reaches the daemon
// byte-for-byte identical to what the operator wrote.
func SubstituteEnvVars(rawCompose []byte, env map[string]string) ([]byte, error) {
	// No `$` anywhere → no interpolation site → nothing to do. Cheap byte
	// scan beats parsing the whole document.
	if !bytes.ContainsRune(rawCompose, '$') {
		return rawCompose, nil
	}

	mapping := template.Mapping(func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	})

	var doc yaml.Node
	if err := yaml.Unmarshal(rawCompose, &doc); err != nil {
		return nil, fmt.Errorf("interpolate ${VAR}: parse yaml: %w", err)
	}
	if err := substituteNode(&doc, mapping); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("interpolate ${VAR}: encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("interpolate ${VAR}: encode yaml: %w", err)
	}
	return buf.Bytes(), nil
}

// substituteNode walks a yaml.Node tree and runs template.Substitute on every
// scalar value. Walks keys too — compose-spec interpolates `${K}: v` keys as
// well. Errors from Substitute (MissingRequiredError, InvalidTemplateError)
// are wrapped with the offending source position so the operator can find the
// site without rereading the whole document.
func substituteNode(n *yaml.Node, mapping template.Mapping) error {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode, yaml.MappingNode:
		for _, c := range n.Content {
			if err := substituteNode(c, mapping); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		// Skip scalars without a `$` — avoids the template regex scan for the
		// overwhelming majority of values that carry no interpolation.
		if strings.IndexByte(n.Value, '$') < 0 {
			return nil
		}
		out, err := template.Substitute(n.Value, mapping)
		if err != nil {
			return fmt.Errorf("interpolate ${VAR} at line %d col %d: %w", n.Line, n.Column, err)
		}
		n.Value = out
	case yaml.AliasNode:
		// Aliases dereference to nodes already walked via their anchor; no
		// per-alias work needed.
	}
	return nil
}
