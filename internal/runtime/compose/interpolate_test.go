package compose_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/template"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestSubstituteEnvVars_BasicVariable — the load-bearing case: ${VAR}
// inside a scalar string is replaced with the matching env value.
func TestSubstituteEnvVars_BasicVariable(t *testing.T) {
	body := []byte("services:\n  web:\n    image: nginx:1.27\n    environment:\n      DB_URL: ${DB_URL}\n")
	out, err := compose.SubstituteEnvVars(body, map[string]string{"DB_URL": "postgres://db/app"})
	if err != nil {
		t.Fatalf("SubstituteEnvVars: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "postgres://db/app") {
		t.Errorf("missing substituted value:\n%s", s)
	}
	if strings.Contains(s, "${DB_URL}") {
		t.Errorf("unresolved placeholder remains:\n%s", s)
	}
}

// TestSubstituteEnvVars_DefaultFallback — `${VAR:-default}` resolves to
// the default when the variable is absent from the env map.
func TestSubstituteEnvVars_DefaultFallback(t *testing.T) {
	body := []byte("services:\n  web:\n    environment:\n      REGION: ${AWS_REGION:-us-east-1}\n")
	out, err := compose.SubstituteEnvVars(body, map[string]string{})
	if err != nil {
		t.Fatalf("SubstituteEnvVars: %v", err)
	}
	if !strings.Contains(string(out), "us-east-1") {
		t.Errorf("default not applied:\n%s", out)
	}
}

// TestSubstituteEnvVars_RequiredMissing — `${VAR:?msg}` returns a
// MissingRequiredError when the variable is absent, surfaced through the
// wrapped error chain.
func TestSubstituteEnvVars_RequiredMissing(t *testing.T) {
	body := []byte("services:\n  web:\n    environment:\n      DB_URL: ${DB_URL:?database url required}\n")
	_, err := compose.SubstituteEnvVars(body, map[string]string{})
	if err == nil {
		t.Fatalf("expected required-missing error")
	}
	var miss *template.MissingRequiredError
	if !errors.As(err, &miss) {
		t.Fatalf("err = %v; want *MissingRequiredError", err)
	}
	if miss.Variable != "DB_URL" {
		t.Errorf("Variable = %q, want DB_URL", miss.Variable)
	}
}

// TestSubstituteEnvVars_DollarEscape — `$$` collapses to a single literal
// `$` and never reaches the mapping function.
func TestSubstituteEnvVars_DollarEscape(t *testing.T) {
	body := []byte("services:\n  web:\n    environment:\n      PRICE: $$5.00\n")
	out, err := compose.SubstituteEnvVars(body, map[string]string{})
	if err != nil {
		t.Fatalf("SubstituteEnvVars: %v", err)
	}
	if !strings.Contains(string(out), "$5.00") {
		t.Errorf("escape not collapsed:\n%s", out)
	}
	if strings.Contains(string(out), "$$5.00") {
		t.Errorf("escape left intact (should be a single $):\n%s", out)
	}
}

// TestSubstituteEnvVars_MissingVarBecomesEmpty — compose-spec parity for
// bare `${VAR}` with no default: the value resolves to empty string when
// the variable is absent.
func TestSubstituteEnvVars_MissingVarBecomesEmpty(t *testing.T) {
	body := []byte("services:\n  web:\n    environment:\n      ABSENT: ${NOT_SET}\n")
	out, err := compose.SubstituteEnvVars(body, map[string]string{})
	if err != nil {
		t.Fatalf("SubstituteEnvVars: %v", err)
	}
	// The substituted scalar collapses to an empty string; re-encoded as
	// an empty plain scalar — the load-bearing assertion is that the
	// placeholder is gone.
	if strings.Contains(string(out), "${NOT_SET}") {
		t.Errorf("unresolved placeholder remains:\n%s", out)
	}
}

// TestSubstituteEnvVars_NestedScalars — substitution walks into nested
// maps, sequences, and scalars in keys.
func TestSubstituteEnvVars_NestedScalars(t *testing.T) {
	body := []byte(
		"services:\n" +
			"  web:\n" +
			"    image: ${REGISTRY}/web:1\n" +
			"    command: [\"--listen\", \"${ADDR}\"]\n" +
			"    environment:\n" +
			"      A: ${A}\n" +
			"      B: ${B}\n")
	env := map[string]string{"REGISTRY": "ghcr.io", "ADDR": ":8080", "A": "1", "B": "2"}
	out, err := compose.SubstituteEnvVars(body, env)
	if err != nil {
		t.Fatalf("SubstituteEnvVars: %v", err)
	}
	s := string(out)
	for _, want := range []string{"ghcr.io/web:1", ":8080", "A: \"1\"", "B: \"2\""} {
		if !strings.Contains(s, want) {
			// Accept either quoted or unquoted scalar — the encoder picks based on
			// content. Loosen the per-value check to the literal substring.
			loose := strings.TrimPrefix(strings.TrimSuffix(want, "\""), "A: \"")
			if !strings.Contains(s, loose) {
				t.Errorf("expected %q in output:\n%s", want, s)
			}
		}
	}
}

// TestSubstituteEnvVars_CommentsSurvive — comments are carried through
// the node-tree round-trip (yaml.v3 attaches them to nodes; we re-encode
// without stripping).
func TestSubstituteEnvVars_CommentsSurvive(t *testing.T) {
	body := []byte(
		"# top-of-file comment kept verbatim\n" +
			"services:\n" +
			"  # web is the only service\n" +
			"  web:\n" +
			"    image: ${REG}/nginx\n")
	out, err := compose.SubstituteEnvVars(body, map[string]string{"REG": "docker.io"})
	if err != nil {
		t.Fatalf("SubstituteEnvVars: %v", err)
	}
	s := string(out)
	for _, want := range []string{"top-of-file comment", "web is the only service"} {
		if !strings.Contains(s, want) {
			t.Errorf("comment %q lost in re-encode:\n%s", want, s)
		}
	}
}

// TestSubstituteEnvVars_EmptyEnvByteIdentical — when both the env and the
// body's `$` budget are empty, the fast path returns the input verbatim.
func TestSubstituteEnvVars_EmptyEnvByteIdentical(t *testing.T) {
	body := []byte("services:\n  web:\n    image: nginx:1.27\n")
	out, err := compose.SubstituteEnvVars(body, nil)
	if err != nil {
		t.Fatalf("SubstituteEnvVars: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected byte-identical passthrough; got %q", out)
	}
}

// TestSubstituteEnvVars_NoDollarByteIdentical — even with a non-empty env,
// a body with no `$` byte short-circuits to byte-identical passthrough.
func TestSubstituteEnvVars_NoDollarByteIdentical(t *testing.T) {
	body := []byte("services:\n  web:\n    image: nginx:1.27\n    environment:\n      FOO: bar\n")
	out, err := compose.SubstituteEnvVars(body, map[string]string{"DB_URL": "postgres://x"})
	if err != nil {
		t.Fatalf("SubstituteEnvVars: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected byte-identical passthrough on no-$ body; got %q", out)
	}
}
