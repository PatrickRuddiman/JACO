package compose_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestValidate_AcceptsNetworkModeNone — issue #121: `network_mode: none`
// passes the closed-field validator with no extra cross-checks. The
// container will have no network at all — that's the operator's intent.
func TestValidate_AcceptsNetworkModeNone(t *testing.T) {
	body := []byte(`services:
  oneshot:
    image: alpine:3.20
    network_mode: none
`)
	if err := compose.Validate(body); err != nil {
		t.Fatalf("Validate: unexpected err: %v", err)
	}
}

// TestValidate_AcceptsNetworkModeServiceInDeployment — issue #121: a
// `network_mode: service:<name>` reference passes when the named service
// is also declared in the same compose document (sidecar pattern).
func TestValidate_AcceptsNetworkModeServiceInDeployment(t *testing.T) {
	body := []byte(`services:
  app:
    image: app:1.0
  sidecar:
    image: vector:0.39
    network_mode: "service:app"
`)
	if err := compose.Validate(body); err != nil {
		t.Fatalf("Validate: unexpected err: %v", err)
	}
}

// TestValidate_RejectsNetworkModeServiceMissing — issue #121: a
// `service:<name>` reference whose target isn't declared in the same
// compose document is rejected with a typed ValidationError that names
// the missing service so the operator can correct the manifest.
func TestValidate_RejectsNetworkModeServiceMissing(t *testing.T) {
	body := []byte(`services:
  sidecar:
    image: vector:0.39
    network_mode: "service:foo"
`)
	err := compose.Validate(body)
	if err == nil {
		t.Fatalf("Validate: expected error for missing target service, got nil")
	}
	var ve *compose.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %T, want *ValidationError: %v", err, err)
	}
	if ve.Code != "validation_failed" {
		t.Errorf("Code = %q, want validation_failed", ve.Code)
	}
	if !strings.Contains(ve.Message, "foo") {
		t.Errorf("Message = %q, want substring naming missing target \"foo\"", ve.Message)
	}
	if ve.Details["target_service"] != "foo" {
		t.Errorf("Details[target_service] = %q, want foo", ve.Details["target_service"])
	}
	if ve.Details["value"] != "service:foo" {
		t.Errorf("Details[value] = %q, want service:foo", ve.Details["value"])
	}
}

// TestValidate_RejectsForbiddenNetworkModes — issue #121: each of the
// docker-compose forms that bypass JACO's per-deployment isolation must
// be rejected with a typed ValidationError naming the offending value so
// the operator sees a clear refusal instead of silent fall-through.
// One sub-test per form so a regression names the exact form that broke.
func TestValidate_RejectsForbiddenNetworkModes(t *testing.T) {
	cases := []struct {
		name string
		mode string
	}{
		{name: "host", mode: "host"},
		{name: "bridge", mode: "bridge"},
		{name: "container_by_id", mode: "container:abc"},
		{name: "named_network", mode: "mynet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte("services:\n  web:\n    image: nginx:1.27\n    network_mode: \"" + tc.mode + "\"\n")
			err := compose.Validate(body)
			if err == nil {
				t.Fatalf("Validate(%s): expected error, got nil", tc.mode)
			}
			var ve *compose.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %T, want *ValidationError: %v", err, err)
			}
			if ve.Code != "validation_failed" {
				t.Errorf("Code = %q, want validation_failed", ve.Code)
			}
			if !strings.Contains(ve.Message, tc.mode) {
				t.Errorf("Message = %q, want substring %q", ve.Message, tc.mode)
			}
			if ve.Details["value"] != tc.mode {
				t.Errorf("Details[value] = %q, want %q", ve.Details["value"], tc.mode)
			}
		})
	}
}

// TestValidate_RejectsBareServicePrefix — issue #121: `service:` with an
// empty target is structurally invalid; reject up front rather than let
// the lifecycle layer trip on a blank lookup. Sister case to the
// missing-target check, kept separate for clarity.
func TestValidate_RejectsBareServicePrefix(t *testing.T) {
	body := []byte(`services:
  sidecar:
    image: vector:0.39
    network_mode: "service:"
`)
	err := compose.Validate(body)
	if err == nil {
		t.Fatalf("Validate: expected error for bare service: prefix")
	}
	var ve *compose.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %T, want *ValidationError: %v", err, err)
	}
	if ve.Code != "validation_failed" {
		t.Errorf("Code = %q, want validation_failed", ve.Code)
	}
}

// TestToContainerSpec_ProjectsNetworkMode — issue #121: every accepted
// form (empty, none, service:<name>) round-trips through ToContainerSpec
// onto ContainerSpec.NetworkMode verbatim. The lifecycle layer is
// responsible for translating `service:<name>` into docker's
// `container:<id>` at create time.
func TestToContainerSpec_ProjectsNetworkMode(t *testing.T) {
	cases := []struct {
		name string
		mode string
	}{
		{name: "empty", mode: ""},
		{name: "none", mode: "none"},
		{name: "service_app", mode: "service:app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := types.ServiceConfig{Image: "x", NetworkMode: tc.mode}
			spec := compose.ToContainerSpec(svc, compose.SpecOptions{
				ClusterID: "c", Deployment: "d", Service: "s", ReplicaID: "r",
			})
			if spec.NetworkMode != tc.mode {
				t.Errorf("NetworkMode = %q, want %q", spec.NetworkMode, tc.mode)
			}
		})
	}
}
