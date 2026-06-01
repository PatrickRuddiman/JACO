package compose_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestValidate_DependsOnListForm — issue #130. Bare list form with an
// existing service passes.
func TestValidate_DependsOnListForm(t *testing.T) {
	body := []byte(`services:
  api:
    image: api:1.0
  web:
    image: nginx:1.27
    depends_on: [api]
`)
	if err := compose.Validate(body); err != nil {
		t.Fatalf("Validate: unexpected err: %v", err)
	}
}

// TestValidate_DependsOnLongFormHealthy — issue #130. Long-form with a
// supported condition passes.
func TestValidate_DependsOnLongFormHealthy(t *testing.T) {
	body := []byte(`services:
  api:
    image: api:1.0
  web:
    image: nginx:1.27
    depends_on:
      api:
        condition: service_healthy
`)
	if err := compose.Validate(body); err != nil {
		t.Fatalf("Validate: unexpected err: %v", err)
	}
}

// TestValidate_RejectsDependsOnSelfReference — issue #130. A service
// depending on itself would deadlock the reconciler gate, so reject at
// validation time with a typed error.
func TestValidate_RejectsDependsOnSelfReference(t *testing.T) {
	body := []byte(`services:
  web:
    image: nginx:1.27
    depends_on: [web]
`)
	err := compose.Validate(body)
	var ve *compose.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if ve.Code != "invalid_depends_on" {
		t.Errorf("Code = %q, want invalid_depends_on", ve.Code)
	}
}

// TestValidate_RejectsDependsOnUndeclaredService — issue #130. Cross-
// deployment refs are out of scope; depending on a service that isn't in
// the same compose document is a manifest bug, not silent acceptance.
func TestValidate_RejectsDependsOnUndeclaredService(t *testing.T) {
	body := []byte(`services:
  web:
    image: nginx:1.27
    depends_on: [missing]
`)
	err := compose.Validate(body)
	var ve *compose.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if ve.Code != "unknown_depends_on_service" {
		t.Errorf("Code = %q, want unknown_depends_on_service", ve.Code)
	}
	if !strings.Contains(ve.Message, "missing") {
		t.Errorf("Message = %q, want substring \"missing\"", ve.Message)
	}
}

// TestValidate_RejectsDependsOnUnsupportedCondition — issue #130. JACO
// doesn't model run-to-completion services; silently accepting
// service_completed_successfully would let dependents wait forever.
func TestValidate_RejectsDependsOnUnsupportedCondition(t *testing.T) {
	body := []byte(`services:
  job:
    image: alpine:3.20
  web:
    image: nginx:1.27
    depends_on:
      job:
        condition: service_completed_successfully
`)
	err := compose.Validate(body)
	var ve *compose.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if ve.Code != "unsupported_depends_on_condition" {
		t.Errorf("Code = %q, want unsupported_depends_on_condition", ve.Code)
	}
	if !strings.Contains(ve.Message, "service_completed_successfully") {
		t.Errorf("Message = %q, want substring naming the bad condition", ve.Message)
	}
}
