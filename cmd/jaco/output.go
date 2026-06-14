package main

import (
	"io"
	"strings"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
)

// renderOutput dispatches on the global --output flag. For json/yaml it
// serializes v via the shared cliclient renderers; for table (the default) it
// runs tableFn, which writes the human-readable view. Commands that call this
// must opt into --output via annotationHonorsOutput so the root guard lets the
// non-table formats through (issue #156).
func renderOutput(out io.Writer, v any, tableFn func() error) error {
	switch flagOutput {
	case "json":
		return cliclient.RenderJSON(out, v)
	case "yaml":
		return cliclient.RenderYAML(out, v)
	default:
		return tableFn()
	}
}

// enumString renders a protobuf enum String() value as the lowercase
// snake_case token used in JSON/YAML output: it strips the closed-set prefix
// (e.g. "REPLICA_STATE_") and lowercases the remainder. This matches the
// convention already established by audit (AuditTypeToString), so structured
// output uses one casing across every command. Table output keeps the
// UPPERCASE form for human scanning.
func enumString(full, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(full, prefix))
}
